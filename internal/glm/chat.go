// chat.go OpenAI 请求 → 清言请求的转换，以及对话转发。
package glm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
)

// assistantIDRe 清言 assistant_id 形状：24 位以上小写 hex（官方智能体 ID）。
var assistantIDRe = regexp.MustCompile(`^[a-f0-9]{24,}$`)

// resolveAssistant 把客户端模型名解析成 (assistant_id, chat_mode)。
//
// 规则（对齐 GLM-Free-API / Chat2API 的映射逻辑）：
//   - 24 位以上 hex → 直接当 assistant_id（清言原生语义，等于可接任意智能体）
//   - 命中静态表 → 该 ID
//   - 名字含 "think"/"zero" → chat_mode = "zero"（推理模型）
//   - 名字含 "deepresearch" → chat_mode = "deep_research"（沉思模型）
//   - 其余 → 默认 assistant_id，chat_mode 为空（普通对话）
func resolveAssistant(model string) (assistantID, chatMode string) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return DefaultAssistantID, ""
	}
	// 剥掉可能的渠道前缀（防御：server 层正常已剥离）
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if m == "" {
		return DefaultAssistantID, ""
	}

	// 剥掉上游模型代号后缀（`:moe_53f` 之类）。
	//
	// 该后缀只用于**展示**服务端实际使用的模型（见 staticModelNames），
	// 路由时必须去掉——否则 `chatglm:moe_53f` 查不到 staticAssistants 里的表项。
	// 保留完整名再查一次表（表里两种键都有），提高容错。
	full := m
	if i := strings.LastIndex(m, ":"); i >= 0 {
		if base := m[:i]; base != "" {
			m = base
		}
	}

	// 模式后缀判定（先于 ID 解析，因为后缀可能附着在任意名字上）
	switch {
	case strings.Contains(m, "deepresearch"):
		chatMode = "deep_research"
	case strings.Contains(m, "think"), strings.Contains(m, "zero"):
		chatMode = "zero"
	}

	// 纯 hex → 当智能体 ID
	if assistantIDRe.MatchString(m) {
		return m, chatMode
	}

	// 特殊智能体：chat_mode 用名字本身标记（见网页版主包常量表）。
	// 必须在静态表查询**之前**判断——否则会被 staticAssistants 命中而丢掉 chat_mode。
	switch m {
	case "ppt":
		return staticAssistants["ppt"], "ppt"
	case "video":
		return staticAssistants["video"], "video"
	}

	// 先查完整名（`chatglm:moe_53f` 这类带后缀的表项），再查剥后缀的基名。
	if id, ok := staticAssistants[full]; ok {
		return id, chatMode
	}
	if id, ok := staticAssistants[m]; ok {
		return id, chatMode
	}
	return DefaultAssistantID, chatMode
}

// glmMessage 清言消息格式。
type glmMessage struct {
	Role    string           `json:"role"`
	Content []glmContentItem `json:"content"`
}

type glmContentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// extractSingleUserPrompt 从 OpenAI messages 提取唯一一条 user 提问。
//
// glm 渠道的对话语义（2026-10-20 简化，用户决议）：**仅支持一来一回的简单对话**——
//   - system 段落：忽略（下游 agent 的系统提示词不透传）；
//   - assistant 段落：**不支持**（报错）——多轮上下文与 tool_calls 都靠它携带，
//     而 glm 通道不支持 tool_calls、也不再拼历史；
//   - 多个 user 段落：**不支持**（报错）；
//   - 唯一 user 的非文本段（image_url 等）忽略，只取 text 拼接。
//
// 报错形态：ChatStream 会把它转成 400 + OpenAI error JSON（code=glm_unsupported_conversation），
// handler 对 ErrBadParams 原文透传不罚号，客户端能直接看到原因。
func extractSingleUserPrompt(msgs []any) (string, error) {
	prompt := ""
	users := 0
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system":
			continue // 忽略 system 段落
		case "assistant":
			return "", fmt.Errorf("glm 渠道不支持多轮对话（不接受 assistant 段落，含 tool_calls 历史）")
		case "user":
			users++
			if users > 1 {
				return "", fmt.Errorf("glm 渠道不支持多轮对话（只接受单个 user 段落，收到 %d 个）", users)
			}
			prompt = strings.TrimSpace(extractText(m["content"]))
		}
	}
	if users == 0 || prompt == "" {
		return "", fmt.Errorf("glm 渠道需要恰好一条含文本的 user 消息")
	}
	return prompt, nil
}

// extractText 从 OpenAI content 提取纯文本（content 可能是字符串或分段数组）。
func extractText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "text" {
				continue
			}
			if s, _ := m["text"].(string); s != "" {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
		return sb.String()
	}
	return ""
}

// openAIUnsupportedBody 构造「不支持多轮/消息形态」的 400 错误体（OpenAI error 形状）。
// code=glm_unsupported_conversation 供 glm.Classify 识别为 ErrBadParams（透传不罚号）。
func openAIUnsupportedBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    "glm_unsupported_conversation",
		},
	})
	return b
}

// glmChatRequest 清言（IDE 通道）对话请求体。
// 字段形状对齐 chatglm-cli ChatBody（抓包 sid 356/480 [实证]）。
type glmChatRequest struct {
	AssistantID    string       `json:"assistant_id"`
	ConversationID string       `json:"conversation_id"` // 恒空串：每次新会话，不续接
	ProjectID      string       `json:"project_id"`
	ChatType       string       `json:"chat_type"`
	Messages       []glmMessage `json:"messages"`
	MetaData       glmChatMeta  `json:"meta_data"`
}

type glmChatMeta struct {
	Channel          string     `json:"channel"`
	ChatMode         string     `json:"chat_mode,omitempty"`
	DraftID          string     `json:"draft_id"`
	Cogview          glmCogview `json:"cogview"`
	IfPlusModel      bool       `json:"if_plus_model"`
	InputQuestionTyp string     `json:"input_question_type"`
	IsNetworking     bool       `json:"is_networking"`
	IsTest           bool       `json:"is_test"`
	Platform         string     `json:"platform"`
	QuoteLogID       string     `json:"quote_log_id"`
	SelectedModel    string     `json:"selected_model"`
}

type glmCogview struct {
	RmLabelWatermark bool   `json:"rm_label_watermark"`
	// AspectRatio 生图比例（1:1/3:4/4:3/9:16/16:9）；对话路径不设（omitempty 不出现）。
	AspectRatio string `json:"aspect_ratio,omitempty"`
}

// ChatStream 把客户端 OpenAI 请求转成清言格式并转发，返回上游 SSE 流。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	// 解析客户端请求（消息校验在拿 token 之前 —— 请求形态问题不应触发刷新/发上游）
	var req struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, 0, nil, fmt.Errorf("glm: 解析请求体失败: %w", err)
	}

	// 提取唯一 user 提问；不支持多轮/tool_calls（见函数注释，报错即 400 透传）。
	prompt, err := extractSingleUserPrompt(req.Messages)
	if err != nil {
		return nil, http.StatusBadRequest, openAIUnsupportedBody(err.Error()), nil
	}
	assistantID, chatMode := resolveAssistant(req.Model)

	// meta 字段逐项对齐 chatglm-cli ChatBody [实证]：
	//   - rm_label_watermark:true 去水印（2026-10-16 A/B 实证 false 产物带「清言·AI生成」角标）；
	//   - if_plus_model:false（IDE 实抓形态，plus_model_available 由服务端回填）；
	//   - platform 写 pc 与 win 指纹的不一致服务端不校验 [实证 10-15]。
	out := glmChatRequest{
		AssistantID:    assistantID,
		ConversationID: "",
		ProjectID:      "",
		ChatType:       "user_chat",
		Messages: []glmMessage{{
			Role:    "user",
			Content: []glmContentItem{{Type: "text", Text: prompt}},
		}},
		MetaData: glmChatMeta{
			Channel:          "",
			ChatMode:         chatMode,
			DraftID:          "",
			Cogview:          glmCogview{RmLabelWatermark: true},
			IfPlusModel:      false,
			InputQuestionTyp: "xxxx",
			IsNetworking:     true,
			IsTest:           false,
			Platform:         "pc",
			QuoteLogID:       "",
			SelectedModel:    chatModelOf(req.Model),
		},
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, 0, nil, err
	}
	return c.postStream(a, payload)
}

// postStream 发 SSE 请求到 EpChat（拿 token、IDE 指纹、cookie jar、StreamHTTP）。
// 对话与生图共用（生图 = 同端点换 assistant_id + cogview 参数）。
func (c *Client) postStream(a *auth.Auth, payload []byte) (io.ReadCloser, int, []byte, error) {
	ctx := context.Background()
	token, err := c.acquireToken(ctx, a)
	if err != nil {
		return nil, 0, nil, err
	}

	// 对话接口必须声明接受 SSE；IDE 指纹头（device_id 从 access JWT 解出）。
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+EpChat, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	c.syncCookies(token)
	for k, v := range ideHeaders(token, "text/event-stream", jwtDeviceID(token)) {
		httpReq.Header.Set(k, v)
	}

	// ⚠️ 流式请求必须用 StreamHTTP（**无 Client.Timeout**）。
	// 用带 Timeout 的 HTTP 会让长回答在超时点被强制掐断、且无终止帧
	// （实测 2026-09-27：GLM 触发 `context deadline exceeded` 5 次）。
	// 见 client.go 中 StreamHTTP 字段的说明。
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	httpReq, cancel := idle.WithCancel(httpReq)
	resp, err := hc.Do(httpReq)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	// 上游可能以 HTTP 200 返回 JSON 错误（非 SSE）
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		cancel()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 尝试解出业务错误
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Status != 0 {
			return nil, http.StatusBadGateway, raw, nil
		}
		return nil, http.StatusBadGateway, raw, nil
	}
	// 成功分支：cancel 所有权交给 idle.Monitor（其 Close 会 cancel；静默超时也会 cancel）。
	return idle.Monitor(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// DeleteConversation 删除上游会话（2026-10-20 换 bulk_delete：IDE 通道实证有效；
// 旧 WEB 端点 backend-api/assistant/conversation/delete 未在 IDE 通道验证，弃用）。
// ⚠️ bulk_delete 的 assistant_id 必须与会话所属 agent 一致，否则上游**静默无效**
// [实证 chatglm-cli]：保活/主对话会话传 DefaultAssistantID，生图会话传 DrawingAssistantID。
// 尽力而为语义由调用方把握（保活路径失败只记日志）。
func (c *Client) DeleteConversation(a *auth.Auth, conversationID, assistantID string) error {
	if conversationID == "" {
		return nil
	}
	if assistantID == "" {
		assistantID = DefaultAssistantID
	}
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"conversation_ids": []string{conversationID},
		"assistant_id":     assistantID,
	})
	_, err = c.doJSON(context.Background(), http.MethodPost, EpBulkDelete, token, json.RawMessage(body))
	return err
}

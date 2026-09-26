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

// buildGLMMessages 把 OpenAI messages 转成清言的单条 user 消息。
//
// 为什么合并成一条：清言服务端无状态，多轮必须由客户端携带全量上下文。
// 参考实现（GLM-Free-API messagesPrepare）也是把全部历史合并进一条 user 消息，
// 并用 <|user|>/<|assistant|> 标记角色边界（这是清言网页版自身的做法）。
//
// 已剥离图像等非文本内容（主对话 content 只收 text）。
func buildGLMMessages(msgs []any) ([]glmMessage, error) {
	var sb strings.Builder
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		text := extractText(m["content"])
		if text == "" {
			continue
		}
		switch role {
		case "system":
			sb.WriteString("<|sytstem|>\n")
		case "assistant":
			sb.WriteString("<|assistant|>\n")
		case "user":
			sb.WriteString("<|user|>\n")
		default:
			sb.WriteString("<|user|>\n")
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return nil, fmt.Errorf("glm: 请求没有可用的文本消息")
	}
	sb.WriteString("<|assistant|>\n")

	// 去掉 Markdown 图片链接与临时路径，避免模型产生幻觉（参考实现同款清理）
	content := mdImageRe.ReplaceAllString(sb.String(), "")
	content = tmpPathRe.ReplaceAllString(content, "")

	return []glmMessage{{
		Role:    "user",
		Content: []glmContentItem{{Type: "text", Text: content}},
	}}, nil
}

var (
	mdImageRe = regexp.MustCompile(`!\[.+\]\(.+\)`)
	tmpPathRe = regexp.MustCompile(`/mnt/data/.+`)
)

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

// glmChatRequest 清言对话请求体。
type glmChatRequest struct {
	AssistantID    string       `json:"assistant_id"`
	ConversationID string       `json:"conversation_id"`
	ProjectID      string       `json:"project_id"`
	ChatType       string       `json:"chat_type"`
	Messages       []glmMessage `json:"messages"`
	MetaData       glmChatMeta  `json:"meta_data"`
}

type glmChatMeta struct {
	Channel          string     `json:"channel"`
	ChatMode         string     `json:"chat_mode,omitempty"`
	DraftID          string     `json:"draft_id"`
	IfPlusModel      bool       `json:"if_plus_model"`
	InputQuestionTyp string     `json:"input_question_type"`
	IsNetworking     bool       `json:"is_networking"`
	IsTest           bool       `json:"is_test"`
	Platform         string     `json:"platform"`
	QuoteLogID       string     `json:"quote_log_id"`
	Cogview          glmCogview `json:"cogview"`
}

type glmCogview struct {
	RmLabelWatermark bool `json:"rm_label_watermark"`
}

// ChatStream 把客户端 OpenAI 请求转成清言格式并转发，返回上游 SSE 流。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	ctx := context.Background()

	token, err := c.acquireToken(ctx, a)
	if err != nil {
		return nil, 0, nil, err
	}

	// 解析客户端请求
	var req struct {
		Model    string `json:"model"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, 0, nil, fmt.Errorf("glm: 解析请求体失败: %w", err)
	}

	msgs, err := buildGLMMessages(req.Messages)
	if err != nil {
		return nil, 0, nil, err
	}
	assistantID, chatMode := resolveAssistant(req.Model)

	out := glmChatRequest{
		AssistantID:    assistantID,
		ConversationID: "",
		ProjectID:      "",
		ChatType:       "user_chat",
		Messages:       msgs,
		MetaData: glmChatMeta{
			Channel:          "",
			ChatMode:         chatMode,
			DraftID:          "",
			IfPlusModel:      true,
			InputQuestionTyp: "xxxx",
			IsNetworking:     true,
			IsTest:           false,
			Platform:         "pc",
			QuoteLogID:       "",
			Cogview:          glmCogview{RmLabelWatermark: false},
		},
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, 0, nil, err
	}

	// 对话接口必须声明接受 SSE
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+EpChat, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, nil, err
	}
	applySignedHeaders(httpReq, token, "text/event-stream")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	// 上游可能以 HTTP 200 返回 JSON 错误（非 SSE）
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		// 尝试解出业务错误
		var env envelope
		if json.Unmarshal(raw, &env) == nil && env.Status != 0 {
			return nil, http.StatusBadGateway, raw, nil
		}
		return nil, http.StatusBadGateway, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// DeleteConversation 删除上游会话，避免在本人的清言对话列表里留痕。
// 尽力而为：失败只记日志，不影响主流程。
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
		"assistant_id":    assistantID,
		"conversation_id": conversationID,
	})
	_, err = c.doJSON(context.Background(), http.MethodPost, EpDeleteConv, token, json.RawMessage(body))
	return err
}

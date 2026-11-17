package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
)

// 对话链路的出站形状（登录链路，同源 ww3 internal/cgpt/chat.go）：
//
//	1 GET  /                                        首页 → data-build 指纹
//	2 POST /backend-api/sentinel/…/prepare          requirements token + PoW + dx
//	3 POST /backend-api/sentinel/…/finalize         PoW + Turnstile → 一次性 token
//	4 POST /backend-api/f/conversation/prepare      conduit（60 秒 TTL，零配额）
//	5 POST /backend-api/f/conversation              **唯一消耗配额的一步**
//
// 前三步由既有预检链路承担（c.EnsureFresh：现取现用的一次性 token、drift 失败即拒），
// 本文件只写第 4、5 步与入站请求形状。
//
// 上游只吃**一条 user 消息**（spec §9）：多轮历史由网关侧压平成一条文本，
// tools/tool_choice/functions 一律忽略（本渠道无工具调用能力，绝不伪造 tool_calls）。
const (
	// EffortOff/EffortAuto/EffortOn 是上游声明的思考档位枚举（仅作提示级，不改 model）。
	EffortOff  = "off"
	EffortAuto = "auto"
	EffortOn   = "on"

	// maxErrBody 是错误响应体的读取上限（与其它渠道同款）。
	maxErrBody = 1 << 20
)

// proxyHint 是普通 403 的可行动提示（spec §10 / §12 P2：不换号、不罚号，原文透传 + 提示）。
const proxyHint = "多半是没配代理或出口 IP 被地区封锁：本渠道必须走代理才能用（配置 HTTPS_PROXY 或渠道代理）"

// inboundChat 是入站 OpenAI Chat 请求里本渠道真正会用到的字段。
type inboundChat struct {
	Model  string       `json:"model"`
	Stream bool         `json:"stream"`
	Effort string       `json:"reasoning_effort"`
	Msgs   []inboundMsg `json:"messages"`
}

// inboundMsg 只取角色与内容；工具/函数字段刻意不解析（本渠道忽略它们）。
type inboundMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// chatRequest 是压平后的出站请求。
//
// History/TurnIdx 是**多轮续接**所需的规范化历史（spec §9 的前缀哈希链）：
// 上游只吃一条 user，但「这一轮接不接上一轮」要靠请求里除最后一条 user 之外
// 的全部消息来判定，所以压平不能把历史丢掉。
type chatRequest struct {
	Prompt  string
	Model   string
	Effort  string
	History []historyMsg // 全部入站消息的规范化形态（按原序，含非 user 角色）
	TurnIdx int          // History 里最后一条 user 的下标（本轮真正发送的那一条）
}

// prefixOf 返回「除本轮 user 之外的全部历史」= 上一轮的落盘键。
func (r *chatRequest) prefixOf() []historyMsg {
	if r.TurnIdx < 0 || r.TurnIdx > len(r.History) {
		return nil
	}
	return r.History[:r.TurnIdx]
}

// throughTurn 返回「历史 + 本轮 user」= 本轮落盘键的前半段。
//
// 后半段是**我们下发给客户端的回复正文**：客户端下一轮会把它原样重放，
// 一起算进键才能让「首句相同的两个不同对话」落到不同条目上（不串话）。
func (r *chatRequest) throughTurn() []historyMsg {
	if r.TurnIdx < 0 || r.TurnIdx >= len(r.History) {
		return nil
	}
	return r.History[:r.TurnIdx+1]
}

// reasoning 指示是否请求上游思考（只有明确 "on" 才发 system_hints:["reason"]）。
func (r *chatRequest) reasoning() bool { return NormalizeEffort(r.Effort) == EffortOn }

// systemHints 返回 system_hints 取值（思考档位只有开/关，未开则不带）。
func (r *chatRequest) systemHints() []string {
	if r.reasoning() {
		return []string{"reason"}
	}
	return nil
}

// NormalizeEffort 把任意客户端的思考档位归一成上游三档。
//
// 归一口径（同源参考实现）：off/none/false/disabled/no → off；
// on/true/yes/low/medium/high/xhigh/max/minimal/ultra → on；
// 其余（含空串、auto、未识别的枚举）→ auto。
// 不报错是刻意的：下游可能按别家枚举发档位，拒收等于把渠道变成「只认一家」。
func NormalizeEffort(e string) string {
	switch strings.ToLower(strings.TrimSpace(e)) {
	case "off", "none", "false", "disabled", "no":
		return EffortOff
	case "on", "true", "yes", "low", "medium", "high", "xhigh", "max", "minimal", "ultra":
		return EffortOn
	default:
		return EffortAuto
	}
}

// parseChatBody 把入站 body 压平成一条 prompt。
//
// 规则（spec §9）：system/developer 消息按顺序前置进第一条 user 文本；
// 正文取**最后一条 user**（其后的 assistant/tool 不参与）；上游只吃一条 user。
func parseChatBody(body []byte) (*chatRequest, error) {
	var in inboundChat
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	hist := make([]historyMsg, 0, len(in.Msgs))
	turnIdx := -1
	var systems []string
	lastUser := ""
	for _, m := range in.Msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := messageText(m.Content)
		hist = append(hist, historyMsg{Role: role, Text: text})
		switch role {
		case "user":
			// 最后一条不是 user（例如 tool 回调）时**向前找最近一条**，不直接报错（spec §9）。
			turnIdx = len(hist) - 1
			lastUser = text
		case "system", "developer":
			if t := strings.TrimSpace(text); t != "" {
				systems = append(systems, t)
			}
		}
	}
	prompt := strings.TrimSpace(lastUser)
	if prompt == "" {
		return nil, fmt.Errorf("没有可发送的用户消息（上游只接受一条 user 文本）")
	}
	if len(systems) > 0 {
		prompt = strings.Join(systems, "\n\n") + "\n\n" + prompt
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		model = defaultChatModel
	}
	return &chatRequest{Prompt: prompt, Model: model, Effort: in.Effort, History: hist, TurnIdx: turnIdx}, nil
}

// messageText 宽容取一条消息的文本（裸串或 text 分片数组）。
//
// 非 text 分片（图片等）一律忽略：本渠道无多模态入站能力，把 base64 当文本
// 发给上游只会炸上下文（R113 同款教训）。
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out strings.Builder
	for _, p := range parts {
		if p.Type == "" || strings.EqualFold(p.Type, "text") {
			out.WriteString(p.Text)
		}
	}
	return out.String()
}

// rejectBody 是本地拒绝（400）的 OpenAI 错误信封。
func rejectBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    "chatgpt_bad_request",
	}})
	return b
}

// ChatStream 走完五步链路后返回**上游原始 SSE 响应**（未解析）。
//
// 约定与其它渠道一致：非 2xx 返回 (nil, status, body, nil) 交给 handler 分类；
// 传输层错误原样返回（handler 判为网络故障：不换号、不计账号错误）；
// 预检/协议形状失败返回 error（handler 直接 502，不换号——换号会撞同一面墙）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	req, err := parseChatBody(body)
	if err != nil {
		return nil, http.StatusBadRequest, rejectBody(err.Error()), nil
	}

	// 步骤 1~3：首页 → prepare → finalize。一次性 token，取不到就不发业务请求。
	sen, err := c.EnsureFresh(a)
	if err != nil {
		return nil, 0, nil, err
	}

	traceID := newUUID()
	// 多轮续接：命中前缀指纹 ⇒ 带上游续接点；未命中 ⇒ parent 落 root = 安静新建会话。
	convID, parentID := c.resumePoint(a, req)
	// 步骤 4：conduit 只有 60 秒寿命且每轮不同 ⇒ 每次请求现取现用。
	conduit, status, raw, err := c.prepareConduit(a, req, traceID, sen, convID, parentID)
	if err != nil {
		return nil, 0, nil, err
	}
	if status != 200 {
		return nil, status, hintForStatus(status, raw), nil
	}

	// 步骤 5：唯一消耗配额的一步。
	payload, err := json.Marshal(conversationBody(req, newUUID(), convID, parentID))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("chatgpt: 对话请求体序列化失败: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := c.OpenStreamCtx(ctx, a, PathFConversation, payload, conversationHeaders(traceID, conduit, sen))
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		cancel()
		return nil, resp.StatusCode, hintForStatus(resp.StatusCode, b), nil
	}
	// 包一层 capture：解析侧（Stream/Aggregate 的 consumeTurn）读完后把本轮的续接点落进链。
	// 只有调用方真的读完才有续接点，所以钩子必须挂在**返回给调用方的这一层**。
	return &convCapture{
		ReadCloser: idle.Monitor(resp.Body, c.IdleTimeout(), cancel),
		chain:      c.convChain(),
		account:    accountKey(a),
		through:    req.throughTurn(),
	}, http.StatusOK, nil, nil
}

// prepareConduit 调 /f/conversation/prepare 拿 conduit_token（零配额）。
func (c *Client) prepareConduit(a *auth.Auth, req *chatRequest, traceID string, sen *Sentinel, convID, parentID string) (string, int, []byte, error) {
	extra := map[string]string{
		"content-type":                            "application/json",
		"x-oai-turn-trace-id":                     traceID,
		"openai-sentinel-chat-requirements-token": sen.ReqToken,
	}
	st, m, raw, err := c.DoJSON(a, "POST", PathFConversationPrepare, conduitBody(req, convID, parentID), extra)
	if err != nil {
		return "", 0, nil, err
	}
	if st != 200 {
		return "", st, raw, nil
	}
	conduit := asString(m["conduit_token"])
	if conduit == "" {
		// 形状漂移：上游没给 conduit 就无法走两段式。属协议层失败，不换号不罚号。
		return "", 0, nil, precheckErr("conversation/prepare 未下发 conduit_token: %s", truncate(string(raw), 400))
	}
	return conduit, 200, nil, nil
}

// conduitBody 构造 conduit 预取的请求体（打字防抖形态，上游接受的两种之一）。
//
// 续接时 parent_message_id 必须与第 5 步一致（conduit 是同一轮的预取，指向别处上游不认）。
func conduitBody(req *chatRequest, convID, parentID string) map[string]any {
	partial := map[string]any{
		"author":  map[string]any{"role": "user"},
		"content": map[string]any{"content_type": "text", "parts": []string{truncate(req.Prompt, conduitPromptCap)}},
		"id":      newUUID(),
	}
	body := map[string]any{
		"client_prepare_state":    "sent",
		"action":                  "next",
		"model":                   req.Model,
		"parent_message_id":       parentOf(parentID),
		"timezone":                defaultTimezone,
		"timezone_offset_min":     defaultTimezoneOffsetMin,
		"is_do_not_remember":      false,
		"local_function_names":    []string{"local.continue_in_work"},
		"partial_query":           partial,
		"client_prepare_dispatch": "debounced",
		"client_prepare_source":   "composer_editor_state",
	}
	if convID != "" {
		body["conversation_id"] = convID
	}
	if hints := req.systemHints(); len(hints) > 0 {
		body["system_hints"] = hints
	}
	return body
}

// parentOf 把「无续接点」统一成上游认识的哨兵值（空串 = 新会话）。
func parentOf(parentID string) string {
	if strings.TrimSpace(parentID) == "" {
		return parentMessageRoot
	}
	return parentID
}

// conversationBody 构造 /f/conversation 的请求体（与真实网页同形）。
//
// 刻意**不写** tools / tool_choice / functions：本渠道无工具能力，伪造会得到
// 一段客户端不认账的文本。
func conversationBody(req *chatRequest, msgID, convID, parentID string) map[string]any {
	meta := map[string]any{
		"automation_creation_attribution": map[string]any{"origin": "conversation", "flow_id": msgID},
	}
	msg := map[string]any{
		"author":      map[string]any{"metadata": map[string]any{}, "name": nil, "role": "user"},
		"channel":     nil,
		"content":     map[string]any{"content_type": "text", "parts": []string{req.Prompt}},
		"create_time": float64(time.Now().UnixMilli()) / 1000,
		"end_turn":    nil,
		"id":          msgID,
		"metadata":    meta,
		"recipient":   "all",
		"status":      "finished_successfully",
		"update_time": nil,
		"weight":      1,
	}
	body := map[string]any{
		"turn_attribution":    map[string]any{"turn_trigger": "composer"},
		"action":              "next",
		"model":               req.Model,
		"parent_message_id":   parentOf(parentID),
		"timezone":            defaultTimezone,
		"timezone_offset_min": defaultTimezoneOffsetMin,
		"is_do_not_remember":  false,
		"client_contextual_info": map[string]any{
			"app_name": "chatgpt.com", "app_surface": "codex_browser",
			"has_web_push_capabilities":        true,
			"web_push_notification_permission": "default",
		},
		"local_function_names":  []string{"local.continue_in_work"},
		"messages":              []any{msg},
		"supported_encodings":   []string{"v1"},
		"genui_state_snapshots": []any{},
		"client_prepare_state":  "success",
	}
	if convID != "" {
		body["conversation_id"] = convID
	}
	// system_hints 必须两处都写（顶层 + messages[0].metadata），只写一处上游不认。
	if hints := req.systemHints(); len(hints) > 0 {
		body["system_hints"] = hints
		meta["system_hints"] = hints
	}
	return body
}

// conversationHeaders 是第 5 步的头集合（conduit + 三个 sentinel token）。
func conversationHeaders(traceID, conduit string, sen *Sentinel) map[string]string {
	extra := map[string]string{
		"content-type":                            "application/json",
		"accept":                                  "*/*",
		"x-conduit-token":                         conduit,
		"x-oai-turn-trace-id":                     traceID,
		"x-openai-web-sse-compression":            "gzip",
		"openai-sentinel-chat-requirements-token": sen.ReqToken,
	}
	for k, v := range sen.Headers() {
		if v != "" {
			extra[k] = v
		}
	}
	return extra
}

// hintForStatus 给普通 403 附一句可行动提示（原文其余字段保持不变）。
//
// 只对「非风控类 403」生效：Unusual activity 是账号级风控，附「没配代理」会误导。
func hintForStatus(status int, body []byte) []byte {
	if status != http.StatusForbidden || Classify(status, string(body)) != provider.ErrPassthrough {
		return body
	}
	m := map[string]any{}
	if json.Unmarshal(body, &m) == nil && m != nil {
		m["hint"] = proxyHint
		if b, err := json.Marshal(m); err == nil {
			return b
		}
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": proxyHint + "；上游原文: " + truncate(string(body), 400),
		"type":    "upstream_error",
		"code":    "chatgpt_forbidden",
	}})
	return b
}

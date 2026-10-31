// anthropic.go Anthropic Messages API（POST /v1/messages）兼容层。
//
// 请求：Anthropic Messages → OpenAI Chat Completions
// 响应：Chat 结果 → Anthropic Messages（非流式）/ SSE（见 anthropic_stream.go）
//
// 客户端来源：Claude Code、各类 Anthropic SDK。鉴权用 x-api-key / Authorization 皆可。
// anthropic-version 请求头忽略（本项目实现的是稳定子集，不做版本分支）。
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"wild-work/internal/provider"
)

// handleAnthropicMessages 处理 POST /v1/messages。
func (g *Gateway) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r, true)
	if ok {
		g.anthropicMessages(w, r, body)
	}
}

func (g *Gateway) anthropicMessages(w http.ResponseWriter, r *http.Request, body map[string]any) {
	model := asString(body["model"])
	resolved, ok := g.resolveModel(w, model, true)
	if !ok {
		return
	}
	// Anthropic 协议要求 max_tokens 必填；缺失时给保守默认值而非报错
	if n, has := asInt(body["max_tokens"]); !has || n <= 0 {
		body["max_tokens"] = 4096
	}

	req, err := anthropicToChat(body, resolved, g.maxTokensCap)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	stream := asBool(body["stream"])

	res, err := g.callChat(w, r, req, stream)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	if stream {
		g.streamAnthropic(w, r, res, model, req)
		return
	}

	status, raw, err := readChatResponse(res)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	if status >= 400 {
		writeUpstreamError(w, status, raw, true)
		return
	}
	chatResp, err := decodeChatResponse(raw)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildAnthropicMessage(chatResp, model))
}

// ---------------------------------------------------------------------------
// 请求转换：Anthropic → Chat Completions
// ---------------------------------------------------------------------------

// anthropicToChat 把 Anthropic Messages 请求体转为 Chat Completions 请求体。
func anthropicToChat(in map[string]any, resolvedModel string, maxTokensCap int) (chatRequest, error) {
	out := chatRequest{"model": resolvedModel, "stream": asBool(in["stream"])}

	var msgs []any
	// system 支持 string 或 content block 数组；Chat 侧只能表达为 system 消息
	if sys := flattenText(in["system"]); strings.TrimSpace(sys) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	// "lastUserContent" 用于补偿部分上游（国际版）要求首条非 user 消息，见 appendLeadingSystem
	rawMsgs, ok := in["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		return nil, fmt.Errorf("`messages` is required and must be a non-empty array")
	}
	for i, m := range rawMsgs {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d] must be an object", i)
		}
		role := strings.ToLower(strings.TrimSpace(asString(mm["role"])))
		converted, err := convertAnthropicMessage(role, mm["content"], i)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}
	out["messages"] = msgs

	copyNumber(out, in, "temperature", "temperature")
	copyNumber(out, in, "top_p", "top_p")
	if n, ok := asInt(in["max_tokens"]); ok && n > 0 {
		out["max_tokens"] = clampMaxTokens(n, maxTokensCap)
	}
	if n, ok := asInt(in["top_k"]); ok && n > 0 {
		out["top_k"] = n // 部分上游支持；不支持的渠道会忽略未知字段
	}
	if stop, ok := in["stop_sequences"].([]any); ok && len(stop) > 0 {
		out["stop"] = stop
	}

	// tools：input_schema → function.parameters
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil {
				continue
			}
			// 服务端工具（如 web_search_20250305）无 Chat 等价物，跳过并记录
			if typ := asString(tm["type"]); typ != "" && !strings.HasPrefix(typ, "custom") && tm["input_schema"] == nil {
				logf("anthropic: 跳过不支持的服务端工具 type=%q name=%q", typ, asString(tm["name"]))
				continue
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        asString(tm["name"]),
					"description": asString(tm["description"]),
					"parameters":  orEmptyObject(tm["input_schema"]),
				},
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	if tc := anthropicToolChoice(in["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	// thinking 参数透传：渠道自行决定是否支持（Qoder 用 thinking.type=enabled）
	if v, has := in["thinking"]; has && !isEmptyValue(v) {
		out["thinking"] = v
		if tm, ok := v.(map[string]any); ok {
			if strings.EqualFold(asString(tm["type"]), "enabled") {
				out["reasoning_effort"] = "medium" // 渠道按 effort 启用思考时的兜底档位
			}
		}
	}
	if md := in["metadata"]; !isEmptyValue(md) {
		if mm, ok := md.(map[string]any); ok {
			if u := asString(mm["user_id"]); u != "" {
				out["user"] = u
			}
		}
	}
	// 上下文档位提示透传（issue #27）：渠道侧解析 context_length/context_window
	for _, k := range []string{"context_length", "context_window"} {
		if n, ok := asInt(in[k]); ok && n > 0 {
			out[k] = n
		}
	}
	return out, nil
}

// convertAnthropicMessage 转换单条 Anthropic 消息，可能产出多条 Chat 消息：
// 含 tool_result 的 user 消息会被拆成若干 role=tool 消息。
func convertAnthropicMessage(role string, content any, idx int) ([]any, error) {
	// content 允许纯字符串简写
	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": normalizeAnthropicRole(role), "content": s}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("messages[%d].content must be a string or an array of blocks", idx)
	}

	var (
		out       []any
		texts     []string
		imgBlocks []any
		toolCalls []any
	)
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(asString(bm["type"])) {
		case "text":
			if t := asString(bm["text"]); t != "" {
				texts = append(texts, t)
			}
		case "image":
			if url := anthropicImageURL(bm); url != "" {
				imgBlocks = append(imgBlocks, imageURLBlock(url, ""))
			}
		case "tool_use":
			// assistant 侧的工具调用 → Chat 的 tool_calls
			args := bm["input"]
			if args == nil {
				args = map[string]any{}
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				argsJSON = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": firstNonEmpty(asString(bm["id"]), "call_"+randSuffix()), "type": "function",
				"function": map[string]any{
					"name": asString(bm["name"]), "arguments": string(argsJSON),
				},
			})
		case "tool_result":
			// user 侧的 tool_result → 独立的 role=tool 消息（Chat 要求工具结果单独成条）
			callID := firstNonEmpty(asString(bm["tool_use_id"]), asString(bm["tool_call_id"]))
			// tool_result 里的图片必须保留成 image_url 块（走 vision），
			// 不能 stringify 成 base64 文本（见 toolContentValue 注释）。
			res := toolContentValue(bm["content"])
			if isTruthy(bm["is_error"]) {
				if txt, ok := res.(string); ok {
					res = "ERROR: " + txt
				}
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": callID, "content": res})
		case "thinking", "redacted_thinking":
			// 思考块无需回传（上游不接受历史 thinking 内容，且对后续推理无信息增量）
		}
	}

	base := normalizeAnthropicRole(role)
	if len(toolCalls) > 0 {
		msg := map[string]any{"role": "assistant", "tool_calls": toolCalls}
		msg["content"] = strings.Join(texts, "\n") // 工具调用与文本可并存
		out = append([]any{msg}, out...)           // assistant 消息必须排在 tool 结果之前
		return out, nil
	}
	// 走到这里说明 out 里只可能有 role=tool 消息（本条消息的工具结果）。
	// 关键不变量：Chat 协议要求 assistant.tool_calls 之后**紧跟**同批 role:tool，
	// 中间插任何一条别的消息都会被上游判「tool calls and tool results do not match」
	// （workbuddy code 11148；qoder 系只回信封层 "Error in upstream response"）。
	// 原先这里用 prepend 把文本插到结果之前，出站顺序变成 user→tool→tool，
	// 正是 Claude Code「工具结果 + 新一轮指令」同条消息时打不开会话的成因。
	// 文本/图片属于「工具执行完之后的新一轮用户输入」，必须排在结果之后。
	if len(texts) == 0 && len(imgBlocks) == 0 {
		return out, nil // 纯工具结果：不再凭空造一条空 user
	}
	var body any
	if len(imgBlocks) > 0 {
		blocks := make([]any, 0, len(texts)+len(imgBlocks))
		for _, t := range texts {
			blocks = append(blocks, map[string]any{"type": "text", "text": t})
		}
		blocks = append(blocks, imgBlocks...)
		body = blocks
	} else {
		body = strings.Join(texts, "\n")
	}
	out = append(out, map[string]any{"role": base, "content": body})
	return out, nil
}

// normalizeAnthropicRole 把 Anthropic 角色归一化为 Chat 角色。
// Anthropic 只有 user/assistant，任何其它值（含缺失）按 user 处理。
func normalizeAnthropicRole(role string) string {
	if strings.EqualFold(role, "assistant") {
		return "assistant"
	}
	return "user"
}

// anthropicImageURL 从 image block 中提取可用的 URL。
// 支持 source.type=url（直链）与 source.type=base64（内联 data URI）。
func anthropicImageURL(block map[string]any) string {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return ""
	}
	if u := asString(src["url"]); u != "" {
		return u
	}
	if data := asString(src["data"]); data != "" {
		media := firstNonEmpty(asString(src["media_type"]), "image/png")
		return "data:" + media + ";base64," + data
	}
	return ""
}

// anthropicToolChoice 把 Anthropic tool_choice 映射为 Chat tool_choice。
//   - {"type":"auto"}                → "auto"
//   - {"type":"any"}                 → "required"
//   - {"type":"tool","name":"x"}     → {"type":"function","function":{"name":"x"}}
//   - {"type":"none"}                → "none"
func anthropicToolChoice(v any) any {
	tc, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch strings.ToLower(asString(tc["type"])) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name := asString(tc["name"]); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 响应转换：Chat 结果 → Anthropic Messages（非流式）
// ---------------------------------------------------------------------------

// buildAnthropicMessage 把 Chat 响应组装为 Anthropic 消息对象。
func buildAnthropicMessage(resp *chatResponse, model string) map[string]any {
	choice := resp.Choices[0]
	text := flattenText(choice.Message.Content)
	if text == "" {
		text = asString(choice.Message.Content)
	}

	content := make([]any, 0, 3)
	// 思考链（thought chain）→ thinking 块；部分思考模型只输出 reasoning_content，
	// 不下发会导致客户端收到空 content。
	if rc := strings.TrimSpace(choice.Message.ReasoningContent); rc != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
	}
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range choice.Message.ToolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    firstNonEmpty(tc.ID, "toolu_"+randSuffix()),
			"name":  tc.Function.Name,
			"input": parseArgsObject(tc.Function.Arguments),
		})
	}

	out := map[string]any{
		"id":          anthropicMessageID(resp.ID),
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": anthropicStopReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0),
	}
	if u := anthropicUsage(resp.Usage); u != nil {
		out["usage"] = u
	}
	return out
}

// parseArgsObject 把工具参数字符串解析为对象。
// Anthropic 的 tool_use.input 必须是对象；解析失败时用 _raw 包装避免丢信息。
func parseArgsObject(args string) any {
	s := strings.TrimSpace(args)
	if s == "" {
		return map[string]any{}
	}
	var obj any
	if json.Unmarshal([]byte(s), &obj) == nil {
		if _, ok := obj.(map[string]any); ok {
			return obj
		}
		return map[string]any{"value": obj}
	}
	return map[string]any{"_raw": s}
}

// anthropicStopReason 把 Chat finish_reason 映射为 Anthropic stop_reason。
//
//	stop           → end_turn
//	length         → max_tokens
//	tool_calls     → tool_use
//	content_filter → end_turn（Anthropic 无对应值，交由客户端按内容判定）
func anthropicStopReason(finish string, hasToolCalls bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop", "":
		if hasToolCalls {
			return "tool_use"
		}
		return "end_turn"
	case "content_filter":
		return "end_turn"
	}
	if hasToolCalls {
		return "tool_use"
	}
	return "end_turn"
}

// anthropicUsage 把 Chat usage 映射为 Anthropic usage（字段名与**语义都不同**）。
//
// 字段名差异：
//
//	OpenAI prompt_tokens    → Anthropic input_tokens（见下，语义需换算）
//	OpenAI completion_tokens→ Anthropic output_tokens
//	OpenAI details.cached_tokens → Anthropic cache_read_input_tokens（命中）
//	OpenAI details.cache_write_tokens → Anthropic cache_creation_input_tokens（写入）
//
// ⚠️ **语义差异（issue #104 的关键陷阱）**：
// Anthropic 的 `input_tokens` 是**未命中、也未写入缓存**的那部分输入，
// 客户端用 `input_tokens + cache_read_input_tokens + cache_creation_input_tokens`
// 还原总输入；而内层的 `prompt_tokens` **已经是总输入**（命中是其子集，见
// internal/server/cacheusage.go 语义 1）。
// 因此必须 `input_tokens = prompt_tokens − cached − write`，否则：
//   - 不填 cache 字段 → Claude Code 把全部输入按未缓存计价（issue #104 的「费用高估」）；
//   - 填了却不减 → 客户端求和得到近两倍总输入（同样错，且方向相反）。
//
// 三者相加恒等于 `prompt_tokens`，与原实现的总量口径一致（只是拆成三段）。
func anthropicUsage(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	in := int64(usageInt(usage, "prompt_tokens", "input_tokens"))
	outTok := usageInt(usage, "completion_tokens", "output_tokens")
	if in == 0 && outTok == 0 {
		return nil
	}
	u := map[string]any{"input_tokens": in, "output_tokens": outTok}
	// 缓存计量：与 /v1/responses、面板同源（internal/provider 唯一真源）。
	c := provider.ExtractUsageCache(usage)
	if c.CachedReported {
		u["cache_read_input_tokens"] = c.Cached
		// 从 input_tokens 里扣除命中部分（Anthropic 口径），钳 0 防坏数据
		if in >= c.Cached {
			u["input_tokens"] = in - c.Cached
		} else {
			u["input_tokens"] = int64(0)
		}
	}
	if c.WriteReported {
		u["cache_creation_input_tokens"] = c.Write
		// 写入部分同样不属于 `input_tokens`（它计在总输入里，但不是「未缓存输入」）
		if cur, _ := u["input_tokens"].(int64); cur >= c.Write {
			u["input_tokens"] = cur - c.Write
		} else {
			u["input_tokens"] = int64(0)
		}
	}
	return u
}

// anthropicMessageID 生成 Anthropic 风格的 msg_ id。
func anthropicMessageID(upstreamID string) string {
	if s := strings.TrimSpace(upstreamID); s != "" {
		if i := strings.Index(s, "-"); i >= 0 && i+1 < len(s) {
			return "msg_" + s[i+1:]
		}
		return "msg_" + s
	}
	return "msg_" + randSuffix()
}

// isTruthy 宽容判定布尔（客户端可能传 true / "true" / 1）。
func isTruthy(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	if n, ok := asInt(v); ok {
		return n != 0
	}
	return asBool(v)
}

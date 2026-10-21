// responses.go OpenAI Responses API（POST /v1/responses）兼容层。
//
// 转换为 Chat Completions 后交给内层，再把 Chat 结果转回 Responses 形状。
//
// 关键规范点（与 tokligence-gateway 的差异所在）：
//   - 工具调用必须是「独立的 function_call output item」，带 call_id（不是塞进 message.content）
//   - 非流式响应必须同时给出 output 数组与 output_text 便利字段
//   - status/incomplete_details 需按 finish_reason 如实映射
//
// 明确不支持（返回 400，避免静默出错）：
//   - previous_response_id（服务端会话续传；本项目无状态，客户端应把历史放进 input）
//
// 容忍并忽略：store / include / truncation / metadata / background（记日志）
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"wild-work/internal/reasoning"
	"wild-work/internal/provider"
)

// handleResponses 处理 POST /v1/responses。
func (g *Gateway) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r, false)
	if !ok {
		return
	}

	if v, has := body["previous_response_id"]; has && strings.TrimSpace(asString(v)) != "" {
		writeOpenAIError(w, http.StatusBadRequest, "unsupported_parameter",
			"previous_response_id is not supported: this gateway is stateless; "+
				"please include full conversation history in `input`")
		return
	}
	for _, k := range []string{"store", "include", "truncation", "metadata", "background"} {
		if v, has := body[k]; has && !isEmptyValue(v) {
			logf("responses: 忽略不支持的参数 %s=%v（无状态实现，不影响对话内容）", k, v)
		}
	}
	// 思考控制先校验再转换：非法/自相矛盾的写法要返回 invalid_reasoning_control，
	// 而不是被当成上游故障。
	if _, rerr := reasoning.Resolve(body, true); rerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_reasoning_control", rerr.Error())
		return
	}

	model := asString(body["model"])
	resolved, ok := g.resolveModel(w, model, false)
	if !ok {
		return
	}
	stream := asBool(body["stream"])

	req, err := responsesToChat(body, resolved, g.maxTokensCap)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	res, err := g.callChat(w, r, req, stream)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_unavailable", err.Error())
		return
	}

	// 思考摘要是否下发：按原始请求体判定（reasoning.summary / include 等字段
	// 在转换后的 Chat 请求体里已被收敛掉），auto 策略下与客户端行为对齐。
	withReasoning := g.summaryEnabled(body)

	if stream {
		g.streamResponses(w, r, res, model, req, withReasoning)
		return
	}

	status, raw, err := readChatResponse(res)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	if status >= 400 {
		writeUpstreamError(w, status, raw, false)
		return
	}
	chatResp, err := decodeChatResponse(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, buildResponsesResponse(chatResp, model, withReasoning))
}

// ---------------------------------------------------------------------------
// 请求转换：Responses → Chat Completions
// ---------------------------------------------------------------------------

// responsesToChat 把 Responses 请求体转为 Chat Completions 请求体。
// resolvedModel 为路由后的 "channel/model"；maxTokensCap>0 时对 max_tokens 封顶。
func responsesToChat(in map[string]any, resolvedModel string, maxTokensCap int) (chatRequest, error) {
	out := chatRequest{"model": resolvedModel, "stream": asBool(in["stream"])}

	var msgs []any

	// input 支持：字符串（单条 user）| 消息数组
	switch v := in["input"].(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": v})
		}
	case []any:
		converted, err := convertResponsesItems(v)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, converted...)
	}

	// 非标准但被部分客户端使用的 messages 别名，直接当作 chat 消息透传
	if extra, ok := in["messages"].([]any); ok {
		msgs = append(extra, msgs...)
	}

	// instructions 等价于 system 提示，置于最前
	if inst := strings.TrimSpace(asString(in["instructions"])); inst != "" {
		msgs = append([]any{map[string]any{"role": "system", "content": inst}}, msgs...)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("`input` is required and must contain at least one message")
	}
	out["messages"] = msgs

	// 采样参数
	copyNumber(out, in, "temperature", "temperature")
	copyNumber(out, in, "top_p", "top_p")

	// max tokens：接受 Responses 的 max_output_tokens 与 Chat 的两个别名，优先级同官方实现
	for _, k := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if n, ok := asInt(in[k]); ok && n > 0 {
			out["max_tokens"] = clampMaxTokens(n, maxTokensCap)
			break
		}
	}

	// tools：Responses 为扁平结构，Chat 需要嵌套 function
	if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
		converted := make([]any, 0, len(tools))
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil {
				continue
			}
			typ := asString(tm["type"])
			if typ != "" && typ != "function" {
				logf("responses: 跳过不支持的 tool 类型 %q（仅支持 function）", typ)
				continue
			}
			converted = append(converted, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        asString(tm["name"]),
					"description": asString(tm["description"]),
					"parameters":  orEmptyObject(tm["parameters"]),
				},
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	if tc, has := in["tool_choice"]; has && !isEmptyValue(tc) {
		out["tool_choice"] = tc // Responses 与 Chat 的 tool_choice 形状兼容（auto/none/required/具体函数）
	}
	if v, ok := in["parallel_tool_calls"].(bool); ok {
		out["parallel_tool_calls"] = v
	}

	// text.format（Responses）→ response_format（Chat）；兼容老式 response_format 直传
	if rf := responsesTextFormat(in); rf != nil {
		out["response_format"] = rf
	} else if v, has := in["response_format"]; has && !isEmptyValue(v) {
		out["response_format"] = v
	}

	// 思考控制：Responses 的标准写法是嵌套 reasoning.effort，同时兼容 Chat 侧的
	// reasoning_effort / thinking.* / enable_thinking 等写法，统一归一化为 reasoning_effort。
	// preferNested=true：两种写法同时出现时以嵌套为准（Responses 协议语义）。
	control, err := reasoning.Resolve(in, true)
	if err != nil {
		return nil, err
	}
	if effort := reasoning.ChatEffort(control); effort != "" {
		out["reasoning_effort"] = effort
	}
	if v, has := in["thinking"]; has && !isEmptyValue(v) {
		out["thinking"] = v // 渠道可能直接识别该对象（如 qoder 的 thinking.type），原样保留
	}
	if u := asString(in["user"]); u != "" {
		out["user"] = u
	}
	// 上下文档位提示透传（issue #27）：渠道侧解析 context_length/context_window
	for _, k := range []string{"context_length", "context_window"} {
		if n, ok := asInt(in[k]); ok && n > 0 {
			out[k] = n
		}
	}
	return out, nil
}

// convertResponsesItems 转换 input 数组的元素（message / function_call / function_call_output / reasoning）。
func convertResponsesItems(items []any) ([]any, error) {
	out := make([]any, 0, len(items))
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input items must be objects, got %T", it)
		}
		typ := strings.ToLower(asString(item["type"]))

		// function_call 必须还原为 assistant.tool_calls，否则紧随其后的 function_call_output
		// 变成「无宿主」的 role=tool 消息，上游会报 11148 tool_call_sequence_broken
		// （Codex CLI 每轮回填都原样回传上一轮的 function_call item）。
		if typ == "function_call" {
			callID := firstNonEmpty(asString(item["call_id"]), asString(item["id"]))
			if callID == "" {
				return nil, fmt.Errorf("function_call item requires call_id")
			}
			// 连续多个 function_call（并行工具调用）合并进同一条 assistant 消息
			if n := len(out); n > 0 {
				if last, ok := out[n-1].(map[string]any); ok && last["role"] == "assistant" && last["tool_calls"] != nil {
					last["tool_calls"] = append(last["tool_calls"].([]any), functionCallToChatToolCall(callID, item))
					continue
				}
			}
			out = append(out, map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{functionCallToChatToolCall(callID, item)},
			})
			continue
		}

		// function_call_output 必须回填为 role=tool 消息，且紧跟其对应的 function_call
		if typ == "function_call_output" {
			callID := firstNonEmpty(asString(item["call_id"]), asString(item["tool_call_id"]))
			if callID == "" {
				return nil, fmt.Errorf("function_call_output item requires call_id")
			}
			out = append(out, map[string]any{
				"role": "tool", "tool_call_id": callID,
				"content": toolContentValue(item["output"]),
			})
			continue
		}
		if typ == "reasoning" {
			continue // 思考链无 Chat 等价物，对后续对话无信息价值，直接丢弃
		}

		role := strings.ToLower(strings.TrimSpace(asString(item["role"])))
		if role == "" {
			// 无 role 但也不是已知的 item 类型：忽略并记录，避免整条请求失败
			logf("responses: 忽略无法识别的 input item type=%q", typ)
			continue
		}

		switch role {
		case "assistant":
			msg := map[string]any{"role": "assistant"}
			if content := convertResponsesContent(item["content"]); content != nil {
				msg["content"] = content
			}
			out = append(out, msg)
		case "system", "developer":
			text := flattenText(item["content"])
			if strings.TrimSpace(text) != "" {
				out = append(out, map[string]any{"role": "system", "content": text})
			}
		case "tool":
			callID := firstNonEmpty(asString(item["tool_call_id"]), asString(item["call_id"]))
			out = append(out, map[string]any{"role": "tool", "tool_call_id": callID, "content": toolContentValue(item["content"])})
		default: // user 及其他未知角色一律按 user 处理
			if content := convertResponsesContent(item["content"]); content != nil {
				out = append(out, map[string]any{"role": "user", "content": content})
			}
		}
	}
	return out, nil
}

// functionCallToChatToolCall 把 Responses 的 function_call item 还原为 Chat 的 tool_calls 元素。
// arguments 在 Responses 里是 JSON 字符串，原样透传即可（上游按字符串解析）。
func functionCallToChatToolCall(callID string, item map[string]any) map[string]any {
	args := asString(item["arguments"])
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return map[string]any{
		"id": callID, "type": "function",
		"function": map[string]any{"name": asString(item["name"]), "arguments": args},
	}
}

// convertResponsesContent 转换消息 content：纯文本 → string；含图片 → Chat 内容块数组。
// 返回 nil 表示内容为空，调用方应跳过该消息。
func convertResponsesContent(content any) any {
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return v
	case []any:
		var (
			texts  []string
			blocks []any
			hasImg bool
		)
		for _, p := range v {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(asString(part["type"])) {
			case "input_text", "output_text", "text", "refusal", "summary_text":
				if t := asString(part["text"]); t != "" {
					texts = append(texts, t)
					blocks = append(blocks, map[string]any{"type": "text", "text": t})
				}
			case "input_image", "image_url", "image":
				url := asString(part["image_url"])
				if url == "" { // 部分客户端用嵌套对象
					if m, ok := part["image_url"].(map[string]any); ok {
						url = asString(m["url"])
					}
				}
				if url == "" {
					url = asString(part["url"])
				}
				if url != "" {
					hasImg = true
					blocks = append(blocks, imageURLBlock(url, ""))
				}
			}
		}
		if !hasImg { // 无图片时降级为纯字符串，兼容只认 string 的上游
			if len(texts) == 0 {
				return nil
			}
			return strings.Join(texts, "\n")
		}
		if len(blocks) == 0 {
			return nil
		}
		return blocks
	}
	return nil
}

// flattenText 把 content 提取为纯文本（供 system/tool 消息使用）。
func flattenText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			if part, ok := p.(map[string]any); ok {
				if t := asString(part["text"]); t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// responsesTextFormat 提取 text.format 并转为 Chat 的 response_format。
func responsesTextFormat(in map[string]any) map[string]any {
	text, _ := in["text"].(map[string]any)
	if text == nil {
		return nil
	}
	format, _ := text["format"].(map[string]any)
	if format == nil {
		return nil
	}
	typ := strings.ToLower(asString(format["type"]))
	switch typ {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		// Responses 的 json_schema 形如 {name, schema, strict}
		js := map[string]any{"name": asString(format["name"]), "schema": orEmptyObject(format["schema"])}
		if strict, ok := format["strict"].(bool); ok {
			js["strict"] = strict
		}
		// 部分客户端直接把 schema 放在 json_schema 字段
		if nm, ok := format["json_schema"].(map[string]any); ok {
			for k, v := range nm {
				js[k] = v
			}
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	default: // "text" 或未知 → 不设置
		return nil
	}
}

// ---------------------------------------------------------------------------
// 响应转换：Chat 结果 → Responses
// ---------------------------------------------------------------------------

// chatResponse 内层非流式响应的结构子集。
type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role             string `json:"role"`
			Content          any    `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

func decodeChatResponse(raw []byte) (*chatResponse, error) {
	var resp chatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("invalid chat response: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("upstream returned no choices")
	}
	return &resp, nil
}

// buildResponsesResponse 把 Chat 响应组装为 Responses 对象。
// model 参数为客户端请求的模型名（回填，便于客户端会话追踪）。
// withReasoning=true 且上游给了思考链时，在 output 首位插入 reasoning item
// （思考先于回答，与官方顺序一致）。
func buildResponsesResponse(resp *chatResponse, model string, withReasoning bool) map[string]any {
	choice := resp.Choices[0]
	text := flattenText(choice.Message.Content)
	if text == "" {
		text = asString(choice.Message.Content)
	}

	output := make([]any, 0, 3)
	if think := strings.TrimSpace(choice.Message.ReasoningContent); withReasoning && think != "" {
		output = append(output, reasoningItem("rs_"+randSuffix(), think, false))
	}
	if strings.TrimSpace(text) != "" {
		output = append(output, messageItem(text))
	}
	for _, tc := range choice.Message.ToolCalls {
		output = append(output, functionCallItem(tc.ID, tc.Function.Name, tc.Function.Arguments))
	}

	out := map[string]any{
		"id":                  responseID(resp.ID),
		"object":              "response",
		"created_at":          nowUnix(),
		"model":               model,
		"status":              "completed",
		"output":              output,
		"output_text":         text, // 便利字段（官方 SDK 亦提供）
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
	}
	if u := responsesUsage(resp.Usage); u != nil {
		out["usage"] = u
	}
	// finish_reason=length → 输出被截断，需如实标记 incomplete
	if strings.EqualFold(choice.FinishReason, "length") {
		out["status"] = "incomplete"
		out["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return out
}

// reasoningItem 构造思考 output item。
//
// 官方形状：summary 为摘要文本数组（type=summary_text），content 为原始思考
// （需 encrypted_content，本项目无状态不做加密，固定给空数组）；
// inProgress=true 时用于 response.output_item.added，summary 留空。
func reasoningItem(id, text string, inProgress bool) map[string]any {
	status := "completed"
	summary := []any{}
	if inProgress {
		status = "in_progress"
	} else {
		summary = append(summary, map[string]any{"type": "summary_text", "text": text})
	}
	return map[string]any{
		"id":                id,
		"type":              "reasoning",
		"status":            status,
		"summary":           summary,
		"content":           []any{},
		"encrypted_content": nil,
	}
}

// messageItem 构造 assistant 文本消息 output item。
func messageItem(text string) map[string]any {
	return map[string]any{
		"id":     "msg_" + randSuffix(),
		"type":   "message",
		"status": "completed",
		"role":   "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{},
		}},
	}
}

// functionCallItem 构造独立的 function_call output item。
// 注意 call_id 与 id 的区别：call_id 用于客户端回传 function_call_output 时匹配。
func functionCallItem(callID, name, args string) map[string]any {
	if strings.TrimSpace(callID) == "" {
		callID = "call_" + randSuffix()
	}
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return map[string]any{
		"id":        "fc_" + randSuffix(),
		"type":      "function_call",
		"status":    "completed",
		"call_id":   callID,
		"name":      name,
		"arguments": args,
	}
}

// responsesUsage 把 Chat usage 映射为 Responses usage（字段名不同）。
func responsesUsage(usage map[string]any) map[string]any {
	if usage == nil {
		return nil
	}
	in := usageInt(usage, "prompt_tokens", "input_tokens")
	outTok := usageInt(usage, "completion_tokens", "output_tokens")
	total := usageInt(usage, "total_tokens")
	if total == 0 {
		total = in + outTok
	}
	if in == 0 && outTok == 0 && total == 0 {
		return nil
	}
	u := map[string]any{"input_tokens": in, "output_tokens": outTok, "total_tokens": total}
	inDetails := map[string]any{} // 只填有值的子字段，避免向客户端输出 null
	outDetails := map[string]any{}
	// 缓存计量：与 /v1/messages、面板**同源**（internal/provider.ExtractUsageCache）。
	// 注：Responses 的 `input_tokens` 与 OpenAI 的 `prompt_tokens` 同为**含缓存口径**
	// （与 Anthropic 的互斥口径不同），故此处**不做**扣减，只映射子字段。
	//
	// 旧实现的两个缺陷在此一并修正：
	//   - 先读顶层裸 `cached_tokens`：workbuddy 系该字段恒 0 是噪声，会把真值掩盖；
	//   - 用 `> 0` 判定：把「上游明确报告本轮未命中（0）」丢成「未提供」（不变量 24）。
	c := provider.ExtractUsageCache(usage)
	if c.CachedReported {
		inDetails["cached_tokens"] = c.Cached
	}
	if reasoning := usageInt(usage, "reasoning_tokens", "completion_thinking_tokens"); reasoning > 0 {
		outDetails["reasoning_tokens"] = reasoning
	} else if d, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if r := usageInt(d, "reasoning_tokens"); r > 0 {
			outDetails["reasoning_tokens"] = r
		}
	}
	if len(inDetails) > 0 {
		u["input_tokens_details"] = inDetails
	}
	if len(outDetails) > 0 {
		u["output_tokens_details"] = outDetails
	}
	return u
}

// writeUpstreamError 把内层透传的上游错误体转成目标协议的错误形状。
// raw 是上游原始 JSON；无法解析时退化为取其中的 message 文本。
func writeUpstreamError(w http.ResponseWriter, status int, raw []byte, anthropicShape bool) {
	msg := extractErrorMessage(raw)
	if anthropicShape {
		writeAnthropicError(w, status, "api_error", msg)
		return
	}
	writeOpenAIError(w, status, "upstream_error", msg)
}

// extractErrorMessage 从上游错误体中提取可读消息（兼容多种形状）。
func extractErrorMessage(raw []byte) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if e, ok := obj["error"].(map[string]any); ok {
			if m := asString(e["message"]); m != "" {
				return m
			}
		}
		if m := asString(obj["message"]); m != "" {
			return m
		}
		if m := asString(obj["msg"]); m != "" {
			return m
		}
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return s
	}
	return "upstream error"
}

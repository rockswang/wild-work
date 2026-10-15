// responses.go MonkeyCode 托管模型里 `type: "openai-responses"` 那一支的
// 请求/响应方言投影。
//
// 为什么单独一支：客户端 settings.json 把托管模型分成两类，实测行为不同
// （2026-09-24，见评估文档 §3.9）：
//
//	anthropic         → POST {base}/messages   签名对象 system[0].text
//	openai-responses  → POST {base}/responses  签名对象 input[0](role=system).content
//
// 把 responses 型模型的请求打到 /messages 会得到 404「路由不存在」；
// 反过来把签名串放进 `instructions` 或顶层 `system` 则一律 403
// （只有 input[0] 的 role=system 条目参与签名校验）。
//
// 响应侧上游回的是 Responses 事件流（`response.output_text.delta` 等），
// 同样要投影成 wild-work 内部统一的 OpenAI Chat SSE。
package monkeycode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 请求投影：OpenAI Chat → OpenAI Responses
// ---------------------------------------------------------------------------

// buildResponsesBody 把 OpenAI Chat 请求体翻译成 Responses 请求体。
//
// 与 Anthropic 面（request.go）的差异：
//   - 系统提示不是独立字段，而是 input 的**第一条** `{role:"system", content:…}`，
//     且这条就是签名对象；
//   - 长度上限字段是 `max_output_tokens`（不是 `max_tokens`）；
//   - 思考用 `reasoning.effort` 表达（**有效**，含 `none` 可关，见 §3.13）；
//     未表达时不下发该字段 = 上游默认档（开）。
func buildResponsesBody(raw []byte) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("monkeycode: invalid request body: %w", err)
	}
	model, _ := in["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("monkeycode: missing model")
	}
	model = upstreamModelName(model)

	items, err := convertResponsesInput(in["messages"])
	if err != nil {
		return nil, err
	}
	// 签名对象恒为 input[0]。
	input := make([]any, 0, len(items)+1)
	input = append(input, map[string]any{"role": "system", "content": signatureSystemPrompt})
	input = append(input, items...)

	out := map[string]any{
		"model":             model,
		"input":             input,
		"max_output_tokens": maxTokens(in),
		// 同 anthropic 面：渠道层恒取 SSE，非流式由网关 Aggregate 收敛。
		"stream": true,
	}
	// 思考档位：上游 `reasoning.effort` 是**有效**的（2026-09-24 VM 实测，见评估文档 §3.13）：
	// `none` → reasoning_tokens **0**（确实能关掉，纠正 §3.9「关不掉」的旧结论）；
	// low/minimal ≈ 130~150；medium/high ≈ 180（易题）~800（难题）。
	// 非法值上游**不报错**、静默回落默认（`bogus` → 293，与不传等价），
	// 因此只转发客户端明确表达的值，既不本地猜测也不静默吞掉。
	// 未表达时**不下发**该字段 —— 上游默认即开，与 anthropic 面「未表达即开」策略一致
	// （2026-09-24 拍板，两面统一）。这里不下发而非补一个默认档，是因为本面的「开」必须
	// 附带档位值，凭空选档等于替客户端决定思考强度；而「不传 = 上游默认档（实测 ≈ medium/high
	// 量级）」已有实测支撑，故沿用。
	if l := reasoningLevel(in); l != "" {
		out["reasoning"] = map[string]any{"effort": l}
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	// 同 anthropic 面：`tool_choice: "none"` 时裁掉整个 tools（见 toolsDisabled）。
	// 上游其实尊重 none（实测未调用），但两面统一处理可少依赖一条上游行为。
	if tools := convertResponsesTools(in["tools"]); len(tools) > 0 && !toolsDisabled(in["tool_choice"]) {
		out["tools"] = tools
		if tc := convertResponsesToolChoice(in["tool_choice"]); tc != nil {
			out["tool_choice"] = tc
		}
	}
	return json.Marshal(out)
}

// convertResponsesInput 把 OpenAI Chat 的 messages 翻成 Responses 的 input 条目。
//
// Responses 的 input 是「条目流」而非纯消息流：工具调用与工具结果都是
// 顶层条目（`function_call` / `function_call_output`），不再是消息里的块。
func convertResponsesInput(raw any) ([]any, error) {
	list, _ := raw.([]any)
	var out []any
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if s := flattenText(m["content"]); strings.TrimSpace(s) != "" {
				out = append(out, map[string]any{"role": "system", "content": s})
			}
		case "assistant":
			out = append(out, responsesAssistantItems(m)...)
		case "tool", "function":
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": responsesCallID(m),
				"output":  flattenText(m["content"]),
			})
		default: // user 及其它未知角色按 user 处理
			if parts := responsesUserParts(m["content"]); len(parts) > 0 {
				out = append(out, map[string]any{"role": "user", "content": parts})
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("monkeycode: 请求里没有可用消息")
	}
	return out, nil
}

// responsesAssistantItems 把一条 assistant 消息拆成「文本消息 + function_call 条目」。
func responsesAssistantItems(m map[string]any) []any {
	var out []any
	blocks := assistantBlocks(m)

	parts := make([]any, 0, len(blocks))
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if bm["type"] == "text" {
			if t, _ := bm["text"].(string); t != "" {
				parts = append(parts, map[string]any{"type": "output_text", "text": t})
			}
		}
	}
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "assistant", "content": parts})
	}
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok || bm["type"] != "tool_use" {
			continue
		}
		args, err := json.Marshal(bm["input"])
		if err != nil || string(args) == "null" {
			args = []byte("{}")
		}
		out = append(out, map[string]any{
			"type":      "function_call",
			"call_id":   bm["id"],
			"name":      bm["name"],
			"arguments": string(args),
		})
	}
	return out
}

// responsesUserParts 转换 user 消息内容为 Responses 的 content 部件。
// 纯文本走字符串形态（上游对两种形态都接受，字符串更省字节）。
func responsesUserParts(v any) []any {
	blocks := contentBlocks(v)
	// 全是纯文本时直接用字符串
	if len(blocks) > 0 {
		allText := true
		var sb strings.Builder
		for _, b := range blocks {
			s, ok := b.(string)
			if !ok {
				allText = false
				break
			}
			sb.WriteString(s)
		}
		if allText && sb.Len() > 0 {
			return []any{map[string]any{"type": "input_text", "text": sb.String()}}
		}
	}
	var out []any
	for _, b := range blocks {
		switch t := b.(type) {
		case string:
			if t != "" {
				out = append(out, map[string]any{"type": "input_text", "text": t})
			}
		case map[string]any:
			switch t["type"] {
			case "text", "":
				if s, ok := t["text"].(string); ok && s != "" {
					out = append(out, map[string]any{"type": "input_text", "text": s})
				}
			case "image_url":
				if img := imageBlock(t); img != nil {
					if src, ok := img["source"].(map[string]any); ok {
						if u, ok := src["url"].(string); ok && u != "" {
							out = append(out, map[string]any{"type": "input_image", "image_url": u})
						}
					}
				}
			}
		}
	}
	return out
}

// responsesCallID 取工具结果要回的调用 ID。
func responsesCallID(m map[string]any) string {
	if id, _ := m["tool_call_id"].(string); id != "" {
		return id
	}
	if id, _ := m["call_id"].(string); id != "" {
		return id
	}
	if n, _ := m["name"].(string); n != "" {
		return n
	}
	return "call_unknown"
}

// convertResponsesTools 把 OpenAI 的 tools 定义转成 Responses 形态。
func convertResponsesTools(raw any) []any {
	list, _ := raw.([]any)
	var out []any
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		tool := map[string]any{"type": "function", "name": name}
		if d, ok := fn["description"].(string); ok && d != "" {
			tool["description"] = d
		}
		if p, ok := fn["parameters"].(map[string]any); ok && len(p) > 0 {
			tool["parameters"] = p
		} else {
			tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, tool)
	}
	return out
}

// convertResponsesToolChoice 把 OpenAI 的 tool_choice 转成 Responses 形态。
func convertResponsesToolChoice(v any) any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "required", "none":
			return t
		case "any":
			return "required"
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return map[string]any{"type": "function", "name": name}
			}
		}
	}
	return nil
}

// flattenText 把内容（字符串或块数组）压成纯文本。
func flattenText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, b := range t {
			switch bm := b.(type) {
			case string:
				sb.WriteString(bm)
			case map[string]any:
				if s, ok := bm["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ---------------------------------------------------------------------------
// 响应投影：Responses SSE → OpenAI Chat SSE
// ---------------------------------------------------------------------------

// responsesStreamReader 逐行读取上游 Responses 事件流，吐出 OpenAI Chat SSE。
type responsesStreamReader struct {
	src   *bufio.Reader
	model string

	pending []byte

	id      string
	created int64

	sentFirst bool

	// toolIdx 把上游 item_id 映射到 OpenAI 的 tool_calls index。
	toolIdx  map[string]int
	nextTool int

	inTokens  int64
	outTokens int64
	// 缓存命中（同 sse.go 的 anthropicStreamReader，见 cacheusage.go）。
	cachedTokens  int64
	cacheReported bool

	finished bool
	done     bool
	probed   bool
}

// newResponsesStream 包装上游响应体；model 为客户端请求的原始模型名（含渠道前缀）。
func newResponsesStream(r io.Reader, model string) io.Reader {
	return &responsesStreamReader{
		src:     bufio.NewReaderSize(r, 64*1024),
		model:   model,
		created: time.Now().Unix(),
		id:      "chatcmpl-" + randomHex(12),
		toolIdx: map[string]int{},
	}
}

// Read 以行为单位吐出转换结果；首次读取先判定 SSE / 单个 JSON（同 sse.go）。
func (s *responsesStreamReader) Read(p []byte) (int, error) {
	if !s.probed {
		s.probed = true
		line, err := s.src.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			rest, _ := io.ReadAll(s.src)
			if s.pending = s.convertResponseJSON(append(trimmed, rest...)); len(s.pending) == 0 {
				s.pending = s.finish("stop")
			}
			return s.drain(p)
		}
		if len(line) > 0 {
			s.pending = s.convertLine(line)
		}
		if err != nil {
			if len(s.pending) == 0 {
				s.pending = s.finish("stop")
			}
			return s.drain(p)
		}
	}
	for len(s.pending) == 0 {
		line, err := s.src.ReadBytes('\n')
		if len(line) > 0 {
			s.pending = s.convertLine(line)
		}
		if err != nil {
			if len(s.pending) == 0 {
				if s.pending = s.finish("stop"); len(s.pending) == 0 {
					return 0, err
				}
			}
			break
		}
	}
	return s.drain(p)
}

func (s *responsesStreamReader) drain(p []byte) (int, error) {
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// convertLine 处理单行上游事件，返回要下发的 OpenAI SSE 文本（可能为空）。
func (s *responsesStreamReader) convertLine(line []byte) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		// `event:` 行与空行丢弃——输出侧自己生成帧边界。
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	typ, _ := ev["type"].(string)

	switch typ {
	case "response.created", "response.in_progress":
		if resp, ok := ev["response"].(map[string]any); ok {
			if id, ok := resp["id"].(string); ok && id != "" {
				s.id = id
			}
			if ts := intField(resp, "created_at"); ts > 0 {
				s.created = ts
			}
		}
		return nil

	case "response.output_item.added":
		item, _ := ev["item"].(map[string]any)
		it, _ := item["type"].(string)
		switch it {
		case "message":
			return s.firstChunk()
		case "function_call":
			id, _ := item["call_id"].(string)
			if id == "" {
				id, _ = item["id"].(string)
			}
			itemID, _ := item["id"].(string)
			name, _ := item["name"].(string)
			idx := s.nextTool
			s.nextTool++
			s.toolIdx[itemID] = idx
			frame := s.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": idx, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": ""},
				}},
			})
			// 部分实现会在 added 事件里直接带完整 arguments
			if args, _ := item["arguments"].(string); args != "" {
				return append(frame, s.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index":    idx,
						"function": map[string]any{"arguments": args},
					}},
				})...)
			}
			return frame
		}
		return nil

	case "response.output_text.delta":
		text, _ := ev["delta"].(string)
		if text == "" {
			return nil
		}
		return s.chunk(map[string]any{"content": text})

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		text, _ := ev["delta"].(string)
		if text == "" {
			return nil
		}
		return s.chunk(map[string]any{"reasoning_content": text})

	case "response.function_call_arguments.delta":
		partial, _ := ev["delta"].(string)
		if partial == "" {
			return nil
		}
		idx, ok := s.toolIdx[stringField(ev, "item_id")]
		if !ok {
			return nil
		}
		return s.chunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    idx,
				"function": map[string]any{"arguments": partial},
			}},
		})

	case "response.completed", "response.incomplete":
		resp, _ := ev["response"].(map[string]any)
		s.captureUsage(resp)
		return s.finish(s.finishReason(resp))

	case "response.failed":
		resp, _ := ev["response"].(map[string]any)
		msg := "upstream response failed"
		if e, ok := resp["error"].(map[string]any); ok {
			if m, _ := e["message"].(string); m != "" {
				msg = m
			}
		}
		return s.errorFrame(msg)

	case "error":
		msg := ""
		if e, ok := ev["error"].(map[string]any); ok {
			msg, _ = e["message"].(string)
			if msg == "" {
				msg, _ = e["type"].(string)
			}
		}
		if msg == "" {
			if m, _ := ev["message"].(string); m != "" {
				msg = m
			}
		}
		if msg == "" {
			msg = "upstream error"
		}
		return s.errorFrame(msg)
	}
	return nil
}

// captureUsage 记录上游 usage（Responses 用 input_tokens / output_tokens）。
func (s *responsesStreamReader) captureUsage(resp map[string]any) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	s.inTokens += intField(u, "input_tokens")
	s.outTokens += intField(u, "output_tokens")
	s.captureCached(u)
}

// finishReason 由 Responses 的 status / incomplete_details 推 OpenAI finish_reason。
func (s *responsesStreamReader) finishReason(resp map[string]any) string {
	if s.nextTool > 0 {
		return "tool_calls"
	}
	if d, ok := resp["incomplete_details"].(map[string]any); ok {
		if r, _ := d["reason"].(string); r == "max_output_tokens" {
			return "length"
		}
	}
	return "stop"
}

// convertResponseJSON 把「单个 Responses JSON 对象」转成等价的 OpenAI SSE。
func (s *responsesStreamReader) convertResponseJSON(raw []byte) []byte {
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	resp := top
	if inner, ok := top["response"].(map[string]any); ok {
		resp = inner
	}
	if e, ok := resp["error"].(map[string]any); ok {
		m, _ := e["message"].(string)
		if m == "" {
			m = "upstream error"
		}
		return s.errorFrame(m)
	}
	if id, ok := resp["id"].(string); ok && id != "" {
		s.id = id
	}
	s.captureUsage(resp)

	var buf bytes.Buffer
	buf.Write(s.firstChunk())
	if items, ok := resp["output"].([]any); ok {
		for _, it := range items {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := im["type"].(string); t {
			case "reasoning":
				for _, sb := range asList(im["summary"]) {
					sm, _ := sb.(map[string]any)
					if txt, _ := sm["text"].(string); txt != "" {
						buf.Write(s.chunk(map[string]any{"reasoning_content": txt}))
					}
				}
			case "message":
				for _, cb := range asList(im["content"]) {
					cm, _ := cb.(map[string]any)
					if cm["type"] != "output_text" {
						continue
					}
					if txt, _ := cm["text"].(string); txt != "" {
						buf.Write(s.chunk(map[string]any{"content": txt}))
					}
				}
			case "function_call":
				callID, _ := im["call_id"].(string)
				if callID == "" {
					callID, _ = im["id"].(string)
				}
				name, _ := im["name"].(string)
				idx := s.nextTool
				s.nextTool++
				buf.Write(s.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index": idx, "id": callID, "type": "function",
						"function": map[string]any{"name": name, "arguments": ""},
					}},
				}))
				if args, _ := im["arguments"].(string); args != "" {
					buf.Write(s.chunk(map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    idx,
							"function": map[string]any{"arguments": args},
						}},
					}))
				}
			}
		}
	}
	buf.Write(s.finish(s.finishReason(resp)))
	return buf.Bytes()
}

// firstChunk 下发首个 chunk（role=assistant）。
func (s *responsesStreamReader) firstChunk() []byte {
	if s.sentFirst {
		return nil
	}
	s.sentFirst = true
	return s.chunk(map[string]any{"role": "assistant", "content": ""})
}

// chunk 生成一个 OpenAI chat.completion.chunk 帧。
func (s *responsesStreamReader) chunk(delta map[string]any) []byte {
	frame := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	}
	return s.encode(frame)
}

// finish 生成收尾帧（finish_reason + usage）并附上 [DONE]。幂等。
func (s *responsesStreamReader) finish(reason string) []byte {
	if s.finished {
		return nil
	}
	s.finished = true
	if reason == "" {
		reason = "stop"
	}
	frame := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
		"usage": map[string]any{
			"prompt_tokens":         s.inTokens,
			"completion_tokens":     s.outTokens,
			"total_tokens":          s.inTokens + s.outTokens,
			"prompt_tokens_details": s.cacheDetails(),
		},
	}
	out := s.encode(frame)
	s.done = true
	return append(out, []byte("data: [DONE]\n\n")...)
}

// errorFrame 透传上游错误（wild-work 的 streamCore 会识别 error 帧）。
func (s *responsesStreamReader) errorFrame(msg string) []byte {
	frame := s.encode(map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}})
	if !s.done {
		s.done = true
		frame = append(frame, []byte("data: [DONE]\n\n")...)
	}
	return frame
}

func (s *responsesStreamReader) encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// asList 把 any 归一成列表（nil → 空）。
func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// stringField 取字符串字段。
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

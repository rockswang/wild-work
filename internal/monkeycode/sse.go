// sse.go 把上游的 Anthropic Messages SSE 流转成 wild-work 内部统一使用的
// OpenAI Chat SSE 流，随后交给 internal/upstream 的 Stream/Aggregate 处理
// （那里已有一整套规范化：tool_calls 分片合并、空 content 噪声剥离、错误帧等）。
//
// 为什么要转而不是让上层直接吃 Anthropic 形状：wild-work 的内层协议恒为
// OpenAI Chat（外层三接口在 internal/gateway 收敛），渠道层负责方言投影。
// 转换后所有既有规范化逻辑零改动复用。
package monkeycode

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// anthropicStreamReader 逐行读取上游 Anthropic SSE，吐出 OpenAI Chat SSE。
// 与 oczen 的 modelRewriter 同构：按行转换、即时吐出，不攒整包。
type anthropicStreamReader struct {
	src   *bufio.Reader
	model string

	pending []byte

	id      string
	created int64
	// sentFirst 标记是否已下发首个 chunk（携带 role=assistant）。
	sentFirst bool

	// toolIdx 把 Anthropic 的 content_block index 映射到 OpenAI 的 tool_calls index。
	toolIdx  map[int]int
	nextTool int

	inTokens  int64
	outTokens int64
	// 缓存命中（上游 input_tokens_details.cached_tokens；cacheReported 区分
	// 「命中 0」与「未上报」，见 cacheusage.go）。
	cachedTokens  int64
	cacheReported bool

	finished bool // 已下发收尾帧
	done     bool // 已吐出 [DONE]

	// probed 标记是否已判定上游响应形态（SSE / 单个 JSON）。
	probed bool
}

// newAnthropicStream 包装上游响应体；model 为客户端请求的原始模型名
// （含渠道前缀），用于回填响应里的 model 字段（R14）。
func newAnthropicStream(r io.Reader, model string) io.Reader {
	return &anthropicStreamReader{
		src:     bufio.NewReaderSize(r, 64*1024),
		model:   model,
		created: time.Now().Unix(),
		id:      "chatcmpl-" + randomHex(12),
		toolIdx: map[int]int{},
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// Read 以行为单位吐出转换结果。
//
// 首次读取时先判定响应形态：正常情况下上游是 SSE（每个事件一行 `data: …`）；
// 但若上游忽略了请求里的 `stream:true` 而回了**单个 Anthropic Messages JSON**，
// 则整包读入后一次性转换——否则转换器会把整份 JSON 当作非 `data:` 行丢掉，
// 表现为「HTTP 200 + 空 content + 0 usage」的**静默失败**（2026-09-24 实测踩到）。
func (s *anthropicStreamReader) Read(p []byte) (int, error) {
	if !s.probed {
		s.probed = true
		line, err := s.src.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			rest, _ := io.ReadAll(s.src)
			if s.pending = s.convertMessageJSON(append(trimmed, rest...)); len(s.pending) == 0 {
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
			// 上游异常收尾：若还没发过收尾帧，补一个，保证客户端能正常结束。
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

// drain 把待吐内容拷进 p。
func (s *anthropicStreamReader) drain(p []byte) (int, error) {
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// convertMessageJSON 把「单个 Anthropic Messages JSON 对象」转成等价的 OpenAI SSE。
//
// 用于上游未按 SSE 回包的兜底路径（见 Read 的注释）。形状：
//
//	{id, type:"message", role:"assistant",
//	 content:[{type:"text",text:…}|{type:"thinking",thinking:…}|{type:"tool_use",…}],
//	 stop_reason:"end_turn", usage:{input_tokens,output_tokens}}
func (s *anthropicStreamReader) convertMessageJSON(raw []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	if e, ok := msg["error"].(map[string]any); ok {
		m, _ := e["message"].(string)
		if m == "" {
			m, _ = e["type"].(string)
		}
		if m == "" {
			m = "upstream error"
		}
		return s.errorFrame(m)
	}
	if id, ok := msg["id"].(string); ok && id != "" {
		s.id = id
	}
	if u, ok := msg["usage"].(map[string]any); ok {
		s.inTokens += intField(u, "input_tokens")
		s.outTokens += intField(u, "output_tokens")
		s.captureCached(u)
	}

	var buf bytes.Buffer
	buf.Write(s.firstChunk())
	if blocks, ok := msg["content"].([]any); ok {
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			bt, _ := bm["type"].(string)
			switch bt {
			case "text":
				if t, _ := bm["text"].(string); t != "" {
					buf.Write(s.chunk(map[string]any{"content": t}))
				}
			case "thinking":
				if t, _ := bm["thinking"].(string); t != "" {
					buf.Write(s.chunk(map[string]any{"reasoning_content": t}))
				}
			case "tool_use":
				name, _ := bm["name"].(string)
				id, _ := bm["id"].(string)
				if id == "" {
					id = "toolu_" + randomHex(8)
				}
				idx := s.nextTool
				s.nextTool++
				s.toolIdx[idx] = idx
				buf.Write(s.chunk(map[string]any{
					"tool_calls": []any{map[string]any{
						"index": idx, "id": id, "type": "function",
						"function": map[string]any{"name": name, "arguments": ""},
					}},
				}))
				if args, err := json.Marshal(bm["input"]); err == nil && string(args) != "null" {
					buf.Write(s.chunk(map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    idx,
							"function": map[string]any{"arguments": string(args)},
						}},
					}))
				}
			}
		}
	}
	stop, _ := msg["stop_reason"].(string)
	buf.Write(s.finish(openAIFinish(stop, s.nextTool > 0)))
	return buf.Bytes()
}

// convertLine 处理单行上游 SSE，返回要下发的 OpenAI SSE 文本（可能为空）。
func (s *anthropicStreamReader) convertLine(line []byte) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		// event: / 注释 / 空行一律丢弃——输出侧自己生成帧边界。
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
	case "message_start":
		if msg, ok := ev["message"].(map[string]any); ok {
			if id, ok := msg["id"].(string); ok && id != "" {
				s.id = id
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				s.inTokens += intField(u, "input_tokens")
				s.captureCached(u)
			}
		}
		return s.firstChunk()

	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		bt, _ := block["type"].(string)
		if bt != "tool_use" {
			// text / thinking 块的开始不需要通知客户端（后续 delta 自带语义）
			return nil
		}
		idx := int(intField(ev, "index"))
		openaiIdx := s.nextTool
		s.nextTool++
		s.toolIdx[idx] = openaiIdx
		name, _ := block["name"].(string)
		id, _ := block["id"].(string)
		if id == "" {
			id = "toolu_" + randomHex(8)
		}
		return s.chunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index": openaiIdx, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}},
		})

	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		dt, _ := delta["type"].(string)
		switch dt {
		case "text_delta":
			text, _ := delta["text"].(string)
			if text == "" {
				return nil
			}
			return s.chunk(map[string]any{"content": text})
		case "thinking_delta":
			text, _ := delta["thinking"].(string)
			if text == "" {
				return nil
			}
			return s.chunk(map[string]any{"reasoning_content": text})
		case "input_json_delta":
			partial, _ := delta["partial_json"].(string)
			if partial == "" {
				return nil
			}
			idx := int(intField(ev, "index"))
			openaiIdx, ok := s.toolIdx[idx]
			if !ok {
				return nil
			}
			return s.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    openaiIdx,
					"function": map[string]any{"arguments": partial},
				}},
			})
		}
		return nil

	case "message_delta":
		if u, ok := ev["usage"].(map[string]any); ok {
			s.outTokens += intField(u, "output_tokens")
		}
		stop := ""
		if delta, ok := ev["delta"].(map[string]any); ok {
			stop, _ = delta["stop_reason"].(string)
		}
		return s.finish(openAIFinish(stop, s.nextTool > 0))

	case "message_stop":
		return s.finish("stop")

	case "error":
		msg := ""
		if e, ok := ev["error"].(map[string]any); ok {
			msg, _ = e["message"].(string)
			if msg == "" {
				msg, _ = e["type"].(string)
			}
		}
		if msg == "" {
			msg = "upstream error"
		}
		return s.errorFrame(msg)
	}
	return nil
}

// firstChunk 下发首个 chunk（role=assistant），OpenAI 客户端依赖它建立消息。
func (s *anthropicStreamReader) firstChunk() []byte {
	if s.sentFirst {
		return nil
	}
	s.sentFirst = true
	return s.chunk(map[string]any{"role": "assistant", "content": ""})
}

// chunk 生成一个 OpenAI chat.completion.chunk 帧。
func (s *anthropicStreamReader) chunk(delta map[string]any) []byte {
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

// finish 生成收尾帧（finish_reason + usage）并附上 [DONE]。幂等：重复调用返回空。
func (s *anthropicStreamReader) finish(reason string) []byte {
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
func (s *anthropicStreamReader) errorFrame(msg string) []byte {
	frame := map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error"}}
	out := s.encode(frame)
	if !s.done {
		s.done = true
		out = append(out, []byte("data: [DONE]\n\n")...)
	}
	return out
}

func (s *anthropicStreamReader) encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// openAIFinish 把 Anthropic 的 stop_reason 映射成 OpenAI 的 finish_reason。
func openAIFinish(stop string, hasTools bool) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "end_turn", "stop_sequence", "":
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	default:
		if hasTools {
			return "tool_calls"
		}
		return "stop"
	}
}

// intField 取数值字段（JSON 数字统一是 float64）。
func intField(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

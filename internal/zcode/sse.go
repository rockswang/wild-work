// sse.go 入站转换：上游的 **Anthropic Messages SSE**（协议 §1.5）→ wild-work 内部
// 统一使用的 **OpenAI Chat SSE**，随后交给 internal/upstream 的 Stream/Aggregate
// 做规范化（那里已有 tool_calls 分片合并、错误帧识别、截断自证等一整套逻辑，转换后零改动复用）。
//
// 为什么不把 Anthropic 形状直接喂给上层：本仓内层协议恒为 OpenAI Chat
// （外层三接口在 internal/gateway 收敛），渠道层负责方言投影。
//
// 事件表（协议 §1.5 的事件名清单，**齐全**）与本文件的对应关系：
//
//	message_start        → 取 message.usage（input / cache_read / cache_creation），下发首个 chunk
//	ping                 → 忽略（保活）
//	content_block_start  → text / thinking 只建状态；tool_use 立即下发 id+name
//	content_block_delta  → text_delta / thinking_delta / input_json_delta（累积 partial_json）
//	content_block_stop   → 交出累积好的块（tool_use 在此一次性给出完整 arguments JSON）
//	message_delta        → 取 delta.stop_reason 与 usage.output_tokens
//	message_stop         → **收尾**（本渠道没有 `[DONE]` 帧）
//	error                → **抛错**（R40：不得伪装正常收尾）
//
// 三个必须记住的坑（协议 §1.5 要点）：
//  1. `signature_delta` **必须忽略**（它是思维链签名，不是正文；当文本下发会把一长串
//     签名注入回答）；
//  2. `thinking` 块映射为 `reasoning_content`（否则思考内容污染正文）；
//  3. 收尾要把缓冲区余量按整行再走一遍（见 Read 的注释），否则被截断的流里最后那条
//     `message_delta` 会丢，而 `stop_reason` 与 usage 都在它上面。
package zcode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// blockState 一个内容块的累积状态。
type blockState struct {
	kind string // thinking / text / tool_use
	// text 累积 thinking / text 块的文本（只用于「块是否为空」的判读；
	// 增量本身是即时下发的，不攒整块）。
	text strings.Builder
	// json 累积 tool_use 的 partial_json。
	json strings.Builder
	name string
	id   string
	// openaiIdx 该 tool_use 在 OpenAI tool_calls 里的下标（由服务端 index 映射而来）。
	openaiIdx int
}

// anthropicStreamReader 逐行读上游 Anthropic SSE，吐出 OpenAI Chat SSE。
// 按行转换、即时吐出，不攒整包。
type anthropicStreamReader struct {
	src   *bufio.Reader
	model string

	pending []byte

	id        string
	created   int64
	sentFirst bool

	// blocks 处于「已 start 未 stop」的块（按服务端下发的 index 索引，
	// **不自己计数**——协议 §1.5：index 直接用服务端下发的）。
	blocks map[int]*blockState
	// nextTool 下一个 OpenAI tool_calls 下标。
	nextTool int
	// blockSeen 是否出现过任何内容块（用于空响应判定）。
	blockSeen bool

	// 累积用量（见 cacheusage.go）。
	inTokens, outTokens          int64
	cachedTokens, writeTokens    int64
	thinkingTokens               int64
	cacheReported, writeReported bool
	thinkingReported             bool

	stopReason string
	finished   bool

	// abnormal 记录传输层中断（连接被掐 / 空闲看门狗判死）。先记下，等已吐出的
	// 内容交付完再上抛，避免「有数据却先报错」把可用内容丢掉。
	abnormal error
	// err 终止性错误：后续 Read 一律返回它（**不补 [DONE]**，R36）。
	err error
}

// newAnthropicStream 包装上游响应体；model 为客户端请求的原始模型名（含渠道前缀），
// 用于回填响应里的 model 字段（R14：上游常返回裸名或 auto）。
func newAnthropicStream(r io.Reader, model string) io.Reader {
	return &anthropicStreamReader{
		src:     bufio.NewReaderSize(r, 64*1024),
		model:   model,
		created: time.Now().Unix(),
		id:      "chatcmpl-" + randomHex(12),
		blocks:  map[int]*blockState{},
	}
}

// Read 以行为单位吐出转换结果。
func (s *anthropicStreamReader) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			// 已判定失败：把错误上抛（**不补 [DONE]**，R36）。
			// upstream 的流式路径收到非 EOF 错误后会写一帧 error 并返回 error，
			// 于是客户端能看到失败原因，而不是「干净地停止、无任何报错」。
			return 0, s.err
		}
		line, err := s.src.ReadBytes('\n')
		if len(line) > 0 {
			// ⚠ **先处理这一行，再判 err**：被截断的流里最后一行（往往正是承载
			// stop_reason 与 usage 的 message_delta）**没有换行符**，bufio.ReadBytes
			// 会把它连同 io.EOF 一起交出来。先判 err 就 return 会整条丢掉它。
			s.pending = s.convertLine(line)
		}
		if err != nil {
			if err != io.EOF && s.abnormal == nil {
				s.abnormal = err
			}
			break // 上游已结束（正常或异常）：不再向它读
		}
	}
	if len(s.pending) > 0 {
		return s.drain(p)
	}
	if s.err == nil {
		s.err = s.finalize() // 可能写入收尾帧，或给出必须上抛的错误
	}
	if len(s.pending) > 0 {
		return s.drain(p)
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

// drain 把待吐内容拷进 p。
func (s *anthropicStreamReader) drain(p []byte) (int, error) {
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// finalize 在「上游流已结束」时决定下一步：补收尾帧，或给出必须上抛的错误。
//
// 三种必须报错的形态（都不补 [DONE]，R36：截断不得伪装正常收尾）：
//   - 传输层中断（连接被掐 / 空闲看门狗判死）→ 流被从中间砍断；
//   - 内容块中途结束（有 content_block_start 却没收 stop）→ 疑似截断，
//     把半个 tool_call arguments 交给客户端只会得到 "tool input was not fully received"；
//   - 一个内容块都没出现过 → **空响应要抛错**（协议 §1.5）。
//
// 正常 EOF（帧完整、只是上游没发 message_stop）不算截断：兜底补收尾帧即可。
func (s *anthropicStreamReader) finalize() error {
	if s.abnormal != nil {
		return fmt.Errorf("zcode：上游流读取中断（%v）", s.abnormal)
	}
	if len(s.blocks) > 0 {
		return fmt.Errorf("zcode：上游流在内容块中途结束（%d 个块未收到 stop，疑似截断）", len(s.blocks))
	}
	if err := s.contentError(); err != nil {
		return err
	}
	s.pending = s.finish(s.stopReason)
	return nil
}

// contentError 空响应判定：一个内容块都没发出 → 错误（协议 §1.5）。
// message_stop 路径与 EOF 路径共用同一个判据，避免两条收尾路径的口径漂移。
func (s *anthropicStreamReader) contentError() error {
	if !s.blockSeen {
		return fmt.Errorf("zcode：上游返回空响应（没有任何内容块）")
	}
	return nil
}

// convertLine 处理单行上游 SSE，返回要下发的 OpenAI SSE 文本（可能为空）。
func (s *anthropicStreamReader) convertLine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	var payload []byte
	switch {
	case bytes.HasPrefix(trimmed, []byte("data:")):
		payload = bytes.TrimSpace(trimmed[len("data:"):])
	case trimmed[0] == '{':
		// 兼容不带 `data:` 前缀的裸 JSON 行（上游形态不完全统一）。
		payload = trimmed
	default:
		// `event:` / `id:` / 注释 / 空行一律丢弃：帧类型由载荷自带的 type 字段决定
		// （协议 §1.5：帧自带 type 字段，同时兼容 event: 行）。
		return nil
	}
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		// 本渠道**没有** `[DONE]` 收尾帧（靠 message_stop）；出现时按空帧忽略，
		// 收尾仍由 message_stop 或 EOF 路径产生，不会因此伪装成正常收尾。
		return nil
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil
	}
	switch strOf(ev["type"]) {
	case "message_start":
		if msg, ok := ev["message"].(map[string]any); ok {
			if id := strOf(msg["id"]); id != "" {
				s.id = id
			}
			if u, ok := msg["usage"].(map[string]any); ok {
				s.captureUsage(u)
			}
		}
		return s.firstChunk()

	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		idx := int(intOf(ev, "index"))
		st := &blockState{
			kind: strOf(block["type"]),
			name: strOf(block["name"]),
			id:   strOf(block["id"]),
		}
		s.blocks[idx] = st
		switch st.kind {
		case "text", "thinking", "redacted_thinking":
			// 开始不下发任何帧：后续 delta 自带语义。
			// ⚠ redacted_thinking 也按 thinking 建状态：它的 delta 里没有可下发的内容
			// （加密思考块），但**必须建状态**，否则后续 content_block_stop 会被当成
			// 孤儿块，finalize 的「块中途结束」判据就会误判截断。
			s.blockSeen = true
			return nil
		case "tool_use":
			s.blockSeen = true
			st.openaiIdx = s.nextTool
			s.nextTool++
			if st.id == "" {
				st.id = "toolu_" + randomHex(8)
			}
			// id / name 在块开始时下发，arguments 在 stop 时下发（两段式，与 OpenAI 惯例一致）。
			return s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": st.openaiIdx, "id": st.id, "type": "function",
				"function": map[string]any{"name": st.name, "arguments": ""},
			}}})
		}
		return nil

	case "content_block_delta":
		idx := int(intOf(ev, "index"))
		st := s.blocks[idx]
		if st == nil {
			return nil
		}
		delta, _ := ev["delta"].(map[string]any)
		switch strOf(delta["type"]) {
		case "text_delta":
			text := strOf(delta["text"])
			if text == "" {
				return nil
			}
			st.text.WriteString(text)
			return s.chunk(map[string]any{"content": text})
		case "thinking_delta":
			text := strOf(delta["thinking"])
			if text == "" {
				return nil
			}
			st.text.WriteString(text)
			// thinking → reasoning_content：**不得混进正文**（协议 §1.5）。
			return s.chunk(map[string]any{"reasoning_content": text})
		case "input_json_delta":
			partial := strOf(delta["partial_json"])
			if partial == "" {
				return nil
			}
			// 累积，到 content_block_stop 一次性交出完整 JSON（避免把半个 JSON 交给客户端）。
			st.json.WriteString(partial)
			return nil
		}
		// signature_delta / redacted_thinking 的 data 落到这里 → **忽略**。
		// ⚠ signature_delta **必须忽略**（协议 §1.5 要点第一条）：它是思维链块签名，
		// 不是正文；一旦当成文本下发，回答里会多出一长串签名。
		return nil

	case "content_block_stop":
		idx := int(intOf(ev, "index"))
		st := s.blocks[idx]
		if st == nil {
			return nil
		}
		delete(s.blocks, idx)
		if st.kind != "tool_use" {
			return nil
		}
		args := strings.TrimSpace(st.json.String())
		if args == "" {
			// 无参工具：给一个完整 JSON，别把空串交出去（客户端会 JSON.parse 失败）。
			args = "{}"
		}
		return s.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index":    st.openaiIdx,
			"function": map[string]any{"arguments": args},
		}}})

	case "message_delta":
		if d, ok := ev["delta"].(map[string]any); ok {
			if sr := strOf(d["stop_reason"]); sr != "" {
				s.stopReason = sr
			}
		}
		if u, ok := ev["usage"].(map[string]any); ok {
			s.captureUsage(u)
		}
		// 这里不下发收尾帧：message_stop 才是终点；若上游没发 message_stop 就断流，
		// finalize 会用已捕获的 stopReason 补收尾帧（**stop_reason 与 usage 都在这一帧上**，
		// 这也是 Read 必须处理无换行尾行的原因）。
		return nil

	case "message_stop":
		// 收尾前先做空响应判定：**message_stop 也不能掩盖「一个块都没发出」**
		// （协议 §1.5：空响应要抛错；否则 UI 表现为「干净地停下、无任何报错」）。
		if err := s.contentError(); err != nil {
			s.err = err
			return nil
		}
		return s.finish(s.stopReason)

	case "error":
		msg := ""
		if e, ok := ev["error"].(map[string]any); ok {
			msg = strOf(e["message"])
			if msg == "" {
				msg = strOf(e["type"])
			}
		}
		if msg == "" {
			msg = "上游未给出错误详情"
		}
		// 流内错误**必须抛错**（协议 §1.5 + R40）：静默收尾会让 UI 表现为
		// 「干净地停止、无任何报错」，客户端会以为这是一次正常的空回答。
		s.err = fmt.Errorf("zcode：上游错误帧：%s", msg)
		return nil

	case "ping":
		return nil
	}
	return nil
}

// firstChunk 下发首个 chunk（role=assistant）：OpenAI 客户端依赖它建立消息。
func (s *anthropicStreamReader) firstChunk() []byte {
	if s.sentFirst {
		return nil
	}
	s.sentFirst = true
	return s.chunk(map[string]any{"role": "assistant", "content": ""})
}

// chunk 生成一个 OpenAI chat.completion.chunk 帧。
func (s *anthropicStreamReader) chunk(delta map[string]any) []byte {
	return s.encode(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": nil,
		}},
	})
}

// finish 生成收尾帧（finish_reason + usage）并附上 [DONE]。幂等：重复调用返回空。
//
// usage 口径见 cacheusage.go：prompt_tokens 是**总输入**（含缓存命中与写入），
// 命中数是它的子集——与 internal/gateway 的 Anthropic 回映射（会做减法）配套。
func (s *anthropicStreamReader) finish(reason string) []byte {
	if s.finished {
		return nil
	}
	s.finished = true
	pt := s.promptTokens()
	usage := map[string]any{
		"prompt_tokens":     pt,
		"completion_tokens": s.outTokens,
		"total_tokens":      pt + s.outTokens,
	}
	if d := s.cacheDetails(); d != nil {
		usage["prompt_tokens_details"] = d
	}
	// 思考 token 是 output 的**子集**，不相加。
	if s.thinkingReported {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": s.thinkingTokens}
	}
	frame := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": openAIFinish(reason),
		}},
		"usage": usage,
	}
	// 本渠道**上游没有 [DONE]**，但本包输出的是 OpenAI 形态，故收尾帧自己补 [DONE]：
	// 这是「转换器的输出协议」要求，不是给上游截断流的遮掩（截断路径一律上抛错误，
	// 见 finalize——那条路径**不会**走到这里）。
	return append(s.encode(frame), []byte("data: [DONE]\n\n")...)
}

// encode 把对象编成 `data: <json>` 帧。
func (s *anthropicStreamReader) encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// openAIFinish 把 Anthropic 的 stop_reason 映射成 OpenAI 的 finish_reason（协议 §1.5）：
//
//	max_tokens → length；tool_use → tool_calls；其余（end_turn / stop_sequence /
//	refusal / 未知 / 缺失）→ stop。
func openAIFinish(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

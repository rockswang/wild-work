// sse.go 入站转换：豆包 **/chat/completion SSE**（主）→ 本仓内层统一使用的 **OpenAI Chat SSE**，
// 随后交给 internal/upstream 的 Stream/Aggregate 做规范化（tool_calls 分片合并、
// 错误帧识别、截断自证等一整套逻辑，转换后零改动复用）。
//
// 与 internal/gemini/sse.go 同范式：渠道层负责方言投影，内层协议恒为 OpenAI Chat。
//
// # 上游 SSE 形态（/chat/completion 工作模式，2026-11-15 实测）
//
//   - 帧块以空行分隔，每块 `event:` 行 + `data:` 行；
//   - `SSE_HEARTBEAT` = 心跳（首帧就是它，跳过）；
//   - `SSE_ACK` = 会话建立（带 conversation_id/section_id，捕获后回写会话缓存，
//     下轮同会话键的请求走增量分支——见 session.go）；
//   - `CHUNK_DELTA` = 正文增量 `{"text":"…"}`（主要正文通道，置 sawChunkDelta）；
//   - `STREAM_CHUNK` = patch 形状，content_block[]：
//     block_type 10000 = 正文（text_block.text，**CHUNK_DELTA 已给过则不再重复吐**）、
//     block_type 10040 = 思考（thinking_block.streaming_title/finish_title → reasoning_content）；
//   - `SSE_REPLY_END` = 收尾，end_type 是**收尾原因非错误码**（正常即 1）；无流式正文时用
//     msg_finish_attr.brief 兜底，仍无则按 R36 上抛；
//   - `STREAM_ERROR` = 流内错误（R40，字段两套都认）。
//   - **遗留兜底**：samantha 方言（event_type 2001/2003/2008…）仍识别，防上游回退；
//     现网主路径已不走 samantha（见 convertEvent 的 default 分支）。
//
// # 已知限制
//
//   - 会话缓存是进程内 map（session.go），重启即失——丢了自动回到首轮全量压平，
//     行为等同旧版；复用被上游拒绝时同理降级。
//   - 上游**不下发 usage**，故本渠道不产出 token 统计（调用方记 0 token + 请求数）。
package doubao

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

// debugRawLimit 原始响应转储的上限（字节）。
const debugRawLimit = 4096

// debugRawOn 是否转储原始响应（环境变量 DOUBAO_DEBUG_RAW=1 开启）。
//
// 为什么需要：协议与上游不一致时，「模型未返回任何内容」这类报错看不出所以然，
// 必须看原始帧。⚠️ 仅调试用：响应不含 token，但可能含对话内容，故默认关闭。
func debugRawOn() bool { return os.Getenv("DOUBAO_DEBUG_RAW") == "1" }

// doubaoStreamReader 逐帧读上游 SSE，吐出 OpenAI Chat SSE。
type doubaoStreamReader struct {
	src   *bufio.Reader
	model string

	// 随聊随删（2026-11-15 拍板，取代复用）：捕获 SSE_ACK 的 conv id，流正常收尾后
	// 0.8s 后台焚化豆包侧会话。sessions 暂存 (key→conv)，key 由调用方按 (账号, 请求键)
	// 预先算好（现在只作焚化关联，不复用）。
	sessions      *sessionStore
	sessionKeyOf_ string
	pendingConvID string // SSE_ACK 捕获的会话 id（随聊随删：流收尾后焚化）

	// 工具文本协议门控（tools.go）：hasTools=true 时 CHUNK_DELTA 正文进缓冲，
	// 识别 <tool_call> 标签转标准 OpenAI tool_calls delta。解析失败降级为正文。
	hasTools bool
	toolGate *toolGate

	pending []byte
	lineBuf []byte // 未成事件块的行缓冲

	id        string
	created   int64
	sentFirst bool

	sawContent    bool
	sawChunkDelta bool // CHUNK_DELTA 已给过正文：STREAM_CHUNK 的 block_type 10000 不再重复吐（参考实现同款去重）
	finished      bool
	abnormal      error
	err           error

	// dbgRaw 原始响应前缀（仅 DOUBAO_DEBUG_RAW=1 时累积）。
	dbgRaw  []byte
	dbgDone bool
}

// newDoubaoStream 包装上游响应体；model 为客户端请求的原始模型名（含渠道前缀）。
func newDoubaoStream(r io.Reader, model string) io.Reader {
	return &doubaoStreamReader{
		src:     bufio.NewReaderSize(r, 64<<10),
		model:   model,
		created: time.Now().Unix(),
		id:      "chatcmpl-doubao-" + randomHex(8),
	}
}

// newDoubaoStreamWithSessions 带会话回写：SSE_ACK 捕获的 conv/section id 会写回缓存。
func newDoubaoStreamWithSessions(r io.Reader, model, sessionKey string, sessions *sessionStore, hasTools bool) io.Reader {
	rdr := newDoubaoStream(r, model).(*doubaoStreamReader)
	rdr.sessions = sessions
	rdr.sessionKeyOf_ = sessionKey
	rdr.hasTools = hasTools
	if hasTools {
		rdr.toolGate = newToolGate()
	}
	return rdr
}

// Read 以帧为单位吐出转换结果。
func (s *doubaoStreamReader) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			s.dumpDebug("err")
			return 0, s.err
		}
		if s.finished {
			s.dumpDebug("eof")
			return 0, io.EOF
		}
		line, err := s.src.ReadBytes('\n')
		if len(line) > 0 {
			s.noteDebug(line)
			s.pending = s.convertLine(line)
		}
		if err != nil {
			if err != io.EOF && s.abnormal == nil {
				s.abnormal = err
			}
			break
		}
	}
	if len(s.pending) > 0 {
		return s.drain(p)
	}
	if s.err == nil && !s.finished {
		s.err = s.finalize()
	}
	if len(s.pending) > 0 {
		return s.drain(p)
	}
	if s.err != nil {
		s.dumpDebug("err")
		return 0, s.err
	}
	s.dumpDebug("eof")
	return 0, io.EOF
}

func (s *doubaoStreamReader) drain(p []byte) (int, error) {
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// finalize 上游流已结束：先把行缓冲里未成事件的残余分发掉（上游可能不发收尾空行），
// 再决定补收尾帧还是上抛错误（R36：截断/空响应不得伪装正常收尾）。
func (s *doubaoStreamReader) finalize() error {
	if s.abnormal != nil {
		return fmt.Errorf("doubao：上游流读取中断（%v）", s.abnormal)
	}
	if len(s.lineBuf) > 0 {
		if s.err == nil {
			s.pending = append(s.pending, s.convertEvent(s.lineBuf)...)
		}
		s.lineBuf = nil
	}
	if s.err != nil {
		return s.err
	}
	// 工具门控 flush：缓冲里若攒着完整调用，转 tool_calls delta（finish_reason=tool_calls）；
	// 解析失败降级为普通正文（绝不丢）。
	if s.toolGate != nil {
		calls, rest := s.toolGate.flush()
		if rest != "" {
			s.pending = append(s.pending, s.chunks(map[string]any{"content": rest})...)
		}
		if len(calls) > 0 {
			s.pending = append(s.pending, s.toolCallFrames(calls)...)
		}
	}
	if !s.sawContent {
		return fmt.Errorf("doubao：模型未返回任何内容")
	}
	// 随聊随删：流正常收尾（正文已完整交付）后焚化本会话。失败流（错误/空响应）
	// 不焚——会话可能未建成或需排查，保守不删（宁留勿错删）。
	if s.pendingConvID != "" {
		s.sessions.forget(s.sessionKeyOf_)
	}
	s.pending = append(s.pending, s.finish()...)
	return nil
}

// convertLine 行缓冲：空行 = 事件块边界。
func (s *doubaoStreamReader) convertLine(line []byte) []byte {
	if len(bytes.TrimSpace(line)) == 0 {
		ev := s.lineBuf
		s.lineBuf = nil
		if len(ev) == 0 {
			return nil
		}
		return s.convertEvent(ev)
	}
	s.lineBuf = append(s.lineBuf, line...)
	return nil
}

// convertEvent 解析一个完整事件块。
func (s *doubaoStreamReader) convertEvent(block []byte) []byte {
	var event string
	var data []string
	for _, raw := range bytes.Split(block, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, ":"):
			// 帧序号 / 注释心跳：忽略
		}
	}
	payload := strings.Join(data, "\n")
	if payload == "" {
		return nil
	}

	switch event {
	case "SSE_HEARTBEAT":
		return nil // 心跳：跳过
	case "gateway-error":
		s.err = fmt.Errorf("doubao：网关级错误 %s", truncateStr(payload, 200))
		return nil
	case "STREAM_ERROR":
		s.err = streamErrorOf(payload)
		return nil
	case "CHUNK_DELTA":
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(payload), &d) == nil && d.Text != "" {
			s.sawChunkDelta = true
			// 工具文本协议门控：hasTools 时正文先进缓冲，是标签则转 tool_calls delta
			if s.toolGate != nil {
				if out := s.toolGate.feed(d.Text); out != "" {
					return s.chunks(map[string]any{"content": out})
				}
				return nil
			}
			return s.chunks(map[string]any{"content": d.Text})
		}
		return nil
	case "STREAM_CHUNK":
		return s.convertStreamChunk(payload)
	case "SSE_ACK":
		// 会话锚点：need_ack_conversation=true 换来的回报（调研报告 §1.3，三个独立实现
		// 的同一契约）。过滤 "0" 占位（SeiShonagon520 同款）。随聊随删：记住 conv id，
		// 流正常收尾后焚化；同时仍写缓存（保持 forget/焚化单一入口，防重复删）。
		var d struct {
			AckClientMeta struct {
				ConversationID string `json:"conversation_id"`
				SectionID      string `json:"section_id"`
			} `json:"ack_client_meta"`
		}
		if json.Unmarshal([]byte(payload), &d) == nil {
			conv := strings.TrimSpace(d.AckClientMeta.ConversationID)
			if conv != "" && conv != "0" {
				s.pendingConvID = conv
				if s.sessions != nil {
					s.sessions.remember(s.sessionKeyOf_, conv, strings.TrimSpace(d.AckClientMeta.SectionID))
				}
			}
		}
		return nil
	case "SSE_REPLY_END":
		var d struct {
			EndType       int `json:"end_type"`
			MsgFinishAttr struct {
				Brief string `json:"brief"`
			} `json:"msg_finish_attr"`
		}
		if json.Unmarshal([]byte(payload), &d) != nil {
			return nil
		}
		// end_type 是「收尾原因」而非错误码——工作模式正常收尾即 end_type=1（2026-11-15
		// 实测：正文完整 + end_type=1）。真正的错误走 STREAM_ERROR / 空响应（R36/R40）。
		// 工具门控模式例外：正文可能全在门控缓冲里（sawContent=false 但缓冲非空），
		// 此处不做空响应判定，交给 finalize 统一 flush 后判定（否则缓冲里的完整调用
		// 会被这里的提前 err 吃掉）。
		if s.toolGate != nil {
			return nil
		}
		var out []byte
		if !s.sawContent {
			// 无流式正文：用 brief 兜底（参考实现同款），仍无则按 R36 上抛、不补 [DONE]。
			if brief := strings.TrimSpace(d.MsgFinishAttr.Brief); brief != "" {
				out = append(out, s.chunks(map[string]any{"content": brief})...)
			} else {
				s.err = fmt.Errorf("doubao：模型未返回任何内容")
				return nil
			}
		}
		// 随聊随删：正常收尾（finish 在此分支发出，finalize 只处理无收尾行的残余）
		// 立即安排焚化。失败流（err 已置）不焚——保守不删（宁留勿错删）。
		if s.err == nil && s.pendingConvID != "" && s.sessions != nil {
			s.sessions.forget(s.sessionKeyOf_)
		}
		out = append(out, s.finish()...)
		return out
	default:
		// samantha 方言：event 为 "message"/"json" 或缺省
		return s.convertSamantha(payload)
	}
}

// convertSamantha 解析 samantha 方言的一帧：{"event_type":N,"event_data":{…}}。
func (s *doubaoStreamReader) convertSamantha(payload string) []byte {
	var frame samanthaFrame
	if json.Unmarshal([]byte(payload), &frame) != nil {
		return nil // 非 JSON 帧忽略
	}
	if frame.EventType == 2005 {
		s.err = fmt.Errorf("doubao：上游流内错误 code=%d msg=%s",
			frame.EventType, truncateStr(samanthaErrText(frame.EventData), 200))
		return nil
	}
	if frame.EventType == 2003 {
		// FIN：结束标记（内容已在之前的 2001 帧里给过）
		if !s.sawContent {
			s.err = fmt.Errorf("doubao：模型未返回任何内容")
			return nil
		}
		return s.finish()
	}

	msg := frame.EventData.Message
	contentType := msg.ContentType
	var out []byte
	switch contentType {
	case 2001, 2003, 10000:
		if t := msg.Content.Text; t != "" {
			out = append(out, s.chunks(map[string]any{"content": t})...)
		}
	case 2008:
		// 深度思考块：think 走 reasoning，text 走正文
		if t := msg.Content.Think; t != "" {
			out = append(out, s.chunks(map[string]any{"reasoning_content": t})...)
		}
		if t := msg.Content.Text; t != "" {
			out = append(out, s.chunks(map[string]any{"content": t})...)
		}
	case 10040:
		t := msg.Content.FinishTitle
		if t == "" {
			t = msg.Content.Title
		}
		if t == "" {
			t = msg.Content.Text
		}
		if t != "" {
			out = append(out, s.chunks(map[string]any{"reasoning_content": t})...)
		}
	case 2002:
		// 推荐问：丢弃
	}
	return out
}

// samanthaFrame samantha 方言的一帧。
//
// ⚠️ **event_data 是 JSON 字符串而不是对象**（2026-11-15 实测：
// `"event_data":"{\"message_id\":...}"`）。Go 的默认解析会把字符串往结构体上
// 套并直接报错，早期版本就是这么把**所有内容帧静默丢弃**的（表现为
// 「模型未返回任何内容」，而上游明明回了 6 帧正文）。故三层都自定义解析：
// 是字符串就再 json.Unmarshal 一次（与参考实现同款容错）。
type samanthaFrame struct {
	EventType int             `json:"event_type"`
	EventData samanthaPayload `json:"event_data"`
}

func (f *samanthaFrame) UnmarshalJSON(b []byte) error {
	var raw struct {
		EventType int             `json:"event_type"`
		EventData json.RawMessage `json:"event_data"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	f.EventType = raw.EventType
	return unmarshalMaybeString(raw.EventData, &f.EventData)
}

type samanthaPayload struct {
	Message samanthaMessage `json:"message"`
}

func (p *samanthaPayload) UnmarshalJSON(b []byte) error {
	var raw struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	return unmarshalMaybeString(raw.Message, &p.Message)
}

type samanthaMessage struct {
	ContentType int `json:"content_type"`
	// Content 可能是对象，也可能是 **JSON 字符串**（上游两种都出现过）。
	Content samanthaContent `json:"content"`
}

func (m *samanthaMessage) UnmarshalJSON(b []byte) error {
	var raw struct {
		ContentType int             `json:"content_type"`
		Content     json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.ContentType = raw.ContentType
	return unmarshalMaybeString(raw.Content, &m.Content)
}

// unmarshalMaybeString 兼容「对象」与「JSON 字符串」两种形态。
//
// 为什么需要：上游在 event_data / message / content 三层都出现过「把 JSON 序列化成
// 字符串再传」的形态；直接按对象解析会静默失败（Go 的 json 对类型不匹配报错，
// 而调用方只看到「解析失败 → 丢弃该帧」），后果是正文整段消失。
func unmarshalMaybeString(raw json.RawMessage, v any) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return err
		}
		if strings.TrimSpace(inner) == "" {
			return nil
		}
		return json.Unmarshal([]byte(inner), v)
	}
	return json.Unmarshal(raw, v)
}

// samanthaContent 自定义 UnmarshalJSON：兼容对象与 JSON 字符串两种形态。
type samanthaContent struct {
	Text        string `json:"text"`
	Think       string `json:"think"`
	FinishTitle string `json:"finish_title"`
	Title       string `json:"title"`
}

func (c *samanthaContent) UnmarshalJSON(b []byte) error {
	type alias samanthaContent
	var a alias
	if err := unmarshalMaybeString(b, &a); err != nil {
		return err
	}
	*c = samanthaContent(a)
	return nil
}

// samanthaErrText 从 event_data 里挖错误文案（字段两套都认）。
func samanthaErrText(p samanthaPayload) string {
	if t := p.Message.Content.Text; t != "" {
		return t
	}
	return ""
}

// streamErrorOf 解析 STREAM_ERROR 帧（字段两套都认）。
func streamErrorOf(payload string) error {
	var d struct {
		Code      int    `json:"code"`
		ErrorCode int    `json:"error_code"`
		Message   string `json:"message"`
		ErrMsg    string `json:"error_msg"`
	}
	_ = json.Unmarshal([]byte(payload), &d)
	code := d.Code
	if code == 0 {
		code = d.ErrorCode
	}
	msg := d.Message
	if msg == "" {
		msg = d.ErrMsg
	}
	if msg == "" {
		msg = truncateStr(payload, 200)
	}
	return fmt.Errorf("doubao：上游流内错误 code=%d msg=%s", code, msg)
}

// convertStreamChunk 旧方言兜底（/chat/completion 的 patch_op 形状）。
func (s *doubaoStreamReader) convertStreamChunk(payload string) []byte {
	var d struct {
		EventType int `json:"event_type"`
		PatchOp   []struct {
			PatchValue struct {
				ContentBlock []struct {
					BlockType int `json:"block_type"`
					Content   struct {
						TextBlock *struct {
							Text string `json:"text"`
						} `json:"text_block"`
						ThinkingBlock *struct {
							StreamingTitle string `json:"streaming_title"`
							FinishTitle    string `json:"finish_title"`
						} `json:"thinking_block"`
					} `json:"content"`
				} `json:"content_block"`
			} `json:"patch_value"`
		} `json:"patch_op"`
	}
	if json.Unmarshal([]byte(payload), &d) != nil {
		return nil
	}
	if d.EventType == 2005 {
		return nil // 交给调用方从 event_data 里看（旧方言极少走这里）
	}
	var out []byte
	for _, op := range d.PatchOp {
		for _, b := range op.PatchValue.ContentBlock {
			switch b.BlockType {
			case 10000:
				if !s.sawChunkDelta && b.Content.TextBlock != nil && b.Content.TextBlock.Text != "" {
					// 工具门控：STREAM_CHUNK 的正文块是本渠道主力通道，与 CHUNK_DELTA 同过门控
					if s.toolGate != nil {
						if gated := s.toolGate.feed(b.Content.TextBlock.Text); gated != "" {
							out = append(out, s.chunks(map[string]any{"content": gated})...)
						}
						continue
					}
					out = append(out, s.chunks(map[string]any{"content": b.Content.TextBlock.Text})...)
				}
			case 10040:
				if b.Content.ThinkingBlock != nil {
					t := b.Content.ThinkingBlock.StreamingTitle
					if t == "" {
						t = b.Content.ThinkingBlock.FinishTitle
					}
					if t != "" {
						out = append(out, s.chunks(map[string]any{"reasoning_content": t})...)
					}
				}
			}
		}
	}
	return out
}

// chunks 下发一个 delta 帧；首个内容帧前补 role=assistant 空帧。
func (s *doubaoStreamReader) chunks(delta map[string]any) []byte {
	var out []byte
	if !s.sentFirst {
		s.sentFirst = true
		out = append(out, s.frame(map[string]any{
			"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
		})...)
	}
	s.sawContent = true
	out = append(out, s.frame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})...)
	return out
}

// toolCallFrames 把解析出的调用转成标准 OpenAI tool_calls delta 帧：
// 首帧 role+tool_calls（name+id），次帧 arguments 全量，finish_reason=tool_calls。
// 标记 finished/sawContent：finish() 的 stop 帧不再补（一次请求只应有一个 finish_reason）。
func (s *doubaoStreamReader) toolCallFrames(calls []parsedToolCall) []byte {
	type tcFn struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type tc struct {
		Index    int    `json:"index"`
		ID       string `json:"id,omitempty"`
		Type     string `json:"type,omitempty"`
		Function tcFn   `json:"function"`
	}
	var out []byte
	// 第一帧：role + tool_calls 骨架
	tcs := make([]tc, 0, len(calls))
	for i, c := range calls {
		tcs = append(tcs, tc{Index: i, ID: newCallID(), Type: "function", Function: tcFn{Name: c.Name}})
	}
	out = append(out, s.frame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": tcs}, "finish_reason": nil}},
	})...)
	// 第二帧：arguments 全量（客户端按 index 拼接）
	tcs2 := make([]tc, 0, len(calls))
	for i, c := range calls {
		tcs2 = append(tcs2, tc{Index: i, Function: tcFn{Arguments: c.Arguments}})
	}
	out = append(out, s.frame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": tcs2}, "finish_reason": nil}},
	})...)
	// 收尾帧：finish_reason=tool_calls
	out = append(out, s.frame(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
	})...)
	out = append(out, []byte("data: [DONE]\n\n")...)
	s.finished = true
	s.sawContent = true
	return out
}

// finish 收尾帧 + [DONE]。幂等。
func (s *doubaoStreamReader) finish() []byte {
	if s.finished {
		return nil
	}
	s.finished = true
	frame := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	return append(s.frame(frame), []byte("data: [DONE]\n\n")...)
}

func (s *doubaoStreamReader) frame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// ---------- 调试转储 ----------

// noteDebug 累积**实际读到的**原始字节（DOUBAO_DEBUG_RAW=1 才有效）。
func (s *doubaoStreamReader) noteDebug(b []byte) {
	if s.dbgDone || !debugRawOn() {
		return
	}
	room := debugRawLimit - len(s.dbgRaw)
	if room <= 0 {
		s.dbgDone = true
		return
	}
	if len(b) > room {
		b = b[:room]
	}
	s.dbgRaw = append(s.dbgRaw, b...)
	if len(s.dbgRaw) >= debugRawLimit {
		s.dbgDone = true
	}
}

// dumpDebug 流结束时输出原始响应前缀。
func (s *doubaoStreamReader) dumpDebug(where string) {
	if !debugRawOn() || s.dbgDone {
		return
	}
	s.dbgDone = true
	if len(s.dbgRaw) == 0 {
		log.Printf("doubao[debug] %s: 上游响应为空", where)
		return
	}
	log.Printf("doubao[debug] %s: 原始响应前 %d 字节:\\n%s", where, len(s.dbgRaw), string(s.dbgRaw))
}

// ---------- 小工具 ----------

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hexDigit = "0123456789abcdef"
	out := make([]byte, 0, n*2)
	for _, c := range b {
		out = append(out, hexDigit[c>>4], hexDigit[c&0x0f])
	}
	return string(out)
}

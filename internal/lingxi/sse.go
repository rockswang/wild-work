package lingxi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// sse.go 灵犀 SSE → OpenAI SSE 转换器（internal/lingxi 包）。
//
// 上游帧序列（lingxi.kdocs.cn，2026-10-28 逐帧实测，实测事实 4）：
//
//	event: execution
//	data: {"type":"execution","data":{...}}
//	event: user
//	data: {"type":"user","data":{...}}
//	event: text_start
//	data: {"type":"text_start","data":{...}}
//	event: text
//	data: {"type":"text","data":"增量纯字符串"}      ← 帧体 data 是纯字符串
//	event: text_end
//	data: {"type":"text_end","data":{...}}
//	event: aigc
//	data: {"type":"end","data":{...}}                 ← 注意 event 名 aigc、帧 type 是 end
//	event: recommend_start / recommend / recommend_end
//	data: {"type":"recommend*","data":{...}}          ← 推荐问题，丢弃
//	event: finish
//	data: ""                                           ← 空 data 终止帧
//
// 变体（magpie 源码支持，实测未复现但保守支持）：
//
//	data: {"type":"reasoning","data":"..."}            → reasoning_content
//	data: {"type":"error","data":"..."|{"message":...}} → 流内错误
//
// 转换规则：type=text/ppt_outline_text→content 增量、type=reasoning→reasoning_content、
// type=end/text_end→finish stop、type=error→StreamError、其余类型丢弃。
// 2026-11-15 实测：Kimi 长上下文回答可只使用 ppt_outline_text 承载正文。
// 帧的 data 字段两种形态：纯字符串直接用；对象取 .text / .message / .error。

// parseSSE 逐行读上游 SSE，把每个完整 data 帧的 payload 交给 onFrame。
// 兼容 event: 行、多行 data 累积、空行分帧（与 glm 同构）。
func parseSSE(r io.Reader, onFrame func(event string, payload []byte) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	var (
		dataBuf  strings.Builder
		eventTyp string
	)

	flush := func() error {
		if dataBuf.Len() == 0 && eventTyp == "" {
			return nil
		}
		payload := dataBuf.String()
		event := eventTyp
		dataBuf.Reset()
		eventTyp = ""
		return onFrame(event, []byte(payload))
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case strings.HasPrefix(trimmed, "event:"):
				eventTyp = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				v := strings.TrimPrefix(trimmed, "data:")
				v = strings.TrimPrefix(v, " ")
				if dataBuf.Len() > 0 {
					dataBuf.WriteString("\n")
				}
				dataBuf.WriteString(v)
			default:
				// id:/retry: 等忽略
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}

// lingxiFrame 上游 data 帧的通用信封：type + data（字符串或对象）。
type lingxiFrame struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// frameText 取帧 data 的文本：纯字符串直接返回；对象取 .text/.message/.error
// 字段（按序取第一个非空）。
func frameText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Text    string `json:"text"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Text != "" {
			return obj.Text
		}
		if obj.Message != "" {
			return obj.Message
		}
		return obj.Error
	}
	return ""
}

// streamConverter 灵犀帧 → OpenAI chat.completion.chunk。
type streamConverter struct {
	model      string
	id         string
	created    int64
	firstChunk bool
	finishSent bool
	sawError   bool
}

func newStreamConverter(model string) *streamConverter {
	return &streamConverter{
		model:      model,
		created:    time.Now().Unix(),
		firstChunk: true,
	}
}

func (sc *streamConverter) chunkID() string {
	if sc.id == "" {
		sc.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return sc.id
}

// writeChunk 写一帧 OpenAI SSE。
func (sc *streamConverter) writeChunk(w io.Writer, delta map[string]any, finishReason any) error {
	chunk := map[string]any{
		"id":      sc.chunkID(),
		"object":  "chat.completion.chunk",
		"created": sc.created,
		"model":   sc.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

// ensureRole 首次发内容前补 role=assistant 帧。
func (sc *streamConverter) ensureRole(w io.Writer, flush func()) error {
	if !sc.firstChunk {
		return nil
	}
	sc.firstChunk = false
	if err := sc.writeChunk(w, map[string]any{"role": "assistant"}, nil); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

// handleFrame 处理一帧：按帧 type 分派。
// 返回 *StreamError 时上层停止转换（error 帧是终态）。
func (sc *streamConverter) handleFrame(w io.Writer, flush func(), payload []byte) error {
	var f lingxiFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		// 无法解析的帧跳过（event 行没有 JSON data 时 payload 为空/非 JSON）。
		return nil
	}

	switch f.Type {
	case "text", "ppt_outline_text":
		text := frameText(f.Data)
		if text == "" {
			return nil
		}
		if err := sc.ensureRole(w, flush); err != nil {
			return err
		}
		if err := sc.writeChunk(w, map[string]any{"content": text}, nil); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
	case "reasoning":
		text := frameText(f.Data)
		if text == "" {
			return nil
		}
		if err := sc.ensureRole(w, flush); err != nil {
			return err
		}
		if err := sc.writeChunk(w, map[string]any{"reasoning_content": text}, nil); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
	case "end", "text_end":
		if !sc.finishSent {
			sc.finishSent = true
			if err := sc.ensureRole(w, flush); err != nil {
				return err
			}
			if err := sc.writeChunk(w, map[string]any{}, "stop"); err != nil {
				return err
			}
			if flush != nil {
				flush()
			}
		}
	case "error":
		// 流内错误帧：终态，转 StreamError 交上层分类处置。
		sc.sawError = true
		msg := frameText(f.Data)
		if msg == "" {
			msg = "lingxi upstream stream error"
		}
		return newStreamError(sc.model, msg)
	default:
		// execution/user/text_start/recommend*/finish 等一律丢弃。
	}
	return nil
}

// Stream 实现 provider.Upstream：灵犀 SSE → OpenAI SSE。
//
// ⚠️ 收尾必须无条件执行（上游中断也补 finish 帧 + [DONE]），否则客户端
// 永远等不到终止帧而挂住（glm hang_test 同款教训）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}

	sc := newStreamConverter(model)
	parseErr := parseSSE(r, func(event string, payload []byte) error {
		return sc.handleFrame(w, flush, payload)
	})

	// error 帧优先返回（该流是失败流，不补 finish 假装成功）。
	if se, ok := parseErr.(*StreamError); ok {
		return nil, se
	}

	// 无条件收尾。
	if !sc.finishSent {
		sc.finishSent = true
		if werr := sc.ensureRole(w, flush); werr != nil && parseErr == nil {
			parseErr = werr
		}
		if werr := sc.writeChunk(w, map[string]any{}, "stop"); werr != nil && parseErr == nil {
			parseErr = werr
		}
	}
	if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil && parseErr == nil {
		parseErr = werr
	}
	flush()
	return nil, parseErr
}

// Aggregate 实现 provider.Upstream：灵犀 SSE 收敛成单个 OpenAI 响应
// （非流式客户端用）。error 帧→返回 StreamError。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		fullText  strings.Builder
		fullThink strings.Builder
		sawError  *StreamError
	)
	err := parseSSE(r, func(event string, payload []byte) error {
		var f lingxiFrame
		if jsonErr := json.Unmarshal(payload, &f); jsonErr != nil {
			return nil
		}
		switch f.Type {
		case "text", "ppt_outline_text":
			fullText.WriteString(frameText(f.Data))
		case "reasoning":
			fullThink.WriteString(frameText(f.Data))
		case "error":
			msg := frameText(f.Data)
			if msg == "" {
				msg = "lingxi upstream stream error"
			}
			sawError = newStreamError(model, msg)
			return sawError // 停止解析
		}
		return nil
	})
	if sawError != nil {
		return nil, sawError
	}
	if err != nil {
		return nil, err
	}

	message := map[string]any{"role": "assistant", "content": fullText.String()}
	if fullThink.Len() > 0 {
		message["reasoning_content"] = fullThink.String()
	}
	return map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
	}, nil
}

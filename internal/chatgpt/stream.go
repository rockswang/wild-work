package chatgpt

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"wild-work/internal/provider"
)

// 出站转换层：把上游「JSON-Patch 增量 SSE」翻成 OpenAI 形状。
//
// 两条路径（流式 / 非流式）共用同一份解析与引用剥离逻辑，唯一差别是投递方式：
//   - Stream 边解析边下发 delta（正文 / 思考两条独立车道）；
//   - Aggregate 攒完再给一次性响应体。
//
// 纪律（spec §10）：
//   - 流被截断必须**自证原因**并报错，**绝不补 [DONE]** 伪装正常收尾；
//   - 上游没有 usage 就**不编造**（返回 nil ⇒ 面板显示「未上报」）；
//   - 工具调用一律不伪造（上游只回纯文本，见 chat.go 的入站压平）。

// errTurnFinished 是「收到 [DONE]」的内部信号，不外泄。
var errTurnFinished = errors.New("chatgpt: turn finished")

// truncatedErr 标记「流被截断」这一**来源**（与信封业务错误区分）：
// 出口据此选 error 帧的 code，不靠嗅探错误文案。
type truncatedErr struct{ msg string }

func (e *truncatedErr) Error() string { return e.msg }

// streamDelta 是一条增量回调（正文增量 + 思考增量，各自可能为空）。
type streamDelta func(text, reason string) error

// consumeTurn 解析上游 SSE 并把增量交给 onDelta。
//
// 返回 (终态, 是否见过任何有效帧, err)。err 非空表示应该向调用方报错
// （业务错误帧 / 截断 / 读中断），此时**不得**再补收尾帧。
func consumeTurn(r io.Reader, onDelta streamDelta) (*turnState, int, error) {
	st := &turnState{}
	frames := 0
	sawDone := false
	err := scanSSE(r, func(payload string) error {
		if payload == "[DONE]" {
			sawDone = true
			return errTurnFinished
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			// 非 JSON 帧（心跳/注释）忽略，但不算有效帧。
			return nil
		}
		if be := businessErrorFrame(ev, payload); be != nil {
			return be
		}
		frames++
		prevText, prevReason := st.Text, st.Reason
		st.apply(ev)
		dt, _ := applyDelta(prevText, st.Text)
		dr, _ := applyDelta(prevReason, st.Reason)
		if dt == "" && dr == "" {
			return nil
		}
		return onDelta(dt, dr)
	})
	if errors.Is(err, errTurnFinished) {
		return st, frames, nil
	}
	if err != nil {
		if _, ok := err.(*provider.Error); ok {
			return st, frames, err
		}
		return st, frames, &truncatedErr{msg: "上游流被截断: " + err.Error()}
	}
	if !sawDone {
		// 读到 EOF 却没有 [DONE]：上游中途断连，内容不可信 ⇒ 报截断（不补收尾）。
		return st, frames, &truncatedErr{msg: "上游流被截断（上游未发 [DONE] 就断连）"}
	}
	return st, frames, nil
}

// businessErrorFrame 识别「HTTP 200 + 信封里的业务错误帧」（截断之外的另一类失败）。
//
// 上游两种形态：`{"type":"error","error":{...}}` 与 `{"error":"文本"}`；
// 分类复用渠道 Classify（同名错误码匹配），但**绝不**落成「可换号」——
// 信封错误说明请求已被账号处理过，换号只会重复同一次失败。
func businessErrorFrame(ev map[string]any, raw string) *provider.Error {
	rawErr, ok := ev["error"]
	if !ok || rawErr == nil {
		return nil
	}
	msg := ""
	code := ""
	if m, ok := rawErr.(map[string]any); ok {
		msg = asString(m["message"])
		code = asString(m["code"])
	}
	if msg == "" {
		msg = asString(rawErr)
	}
	if msg == "" {
		msg = raw
	}
	if code != "" {
		msg = "[" + code + "] " + msg
	}
	kind := Classify(0, raw)
	if kind == provider.ErrClient {
		// status 0 时 Classify 无从判断，回落到「上游错误」：不换号、不计账号错误。
		kind = provider.ErrUpstream
	}
	return &provider.Error{Kind: kind, Status: http.StatusBadGateway, Msg: "上游错误: " + msg}
}

// sseWriter 是 OpenAI 形状的 SSE 出口（首次写 delta 时才补 role）。
type sseWriter struct {
	w        http.ResponseWriter
	fl       http.Flusher
	id       string
	model    string
	created  int64
	roleSent bool
	started  bool
}

func newSSEWriter(w http.ResponseWriter, model string) *sseWriter {
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl, id: "chatcmpl-" + newUUID(), model: model, created: time.Now().Unix()}
}

// start 写 SSE 响应头（幂等；报错路径会先 clearSSEHeaders 再写 JSON）。
func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

func (s *sseWriter) flush() {
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *sseWriter) writeEvent(payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.flush()
}

// delta 下发一条增量（text → delta.content，reason → delta.reasoning_content）。
func (s *sseWriter) delta(text, reason string) error {
	if text == "" && reason == "" && s.roleSent {
		return nil
	}
	s.start()
	d := map[string]any{}
	if !s.roleSent {
		d["role"] = "assistant"
		s.roleSent = true
	}
	if text != "" {
		d["content"] = text
	}
	if reason != "" {
		d["reasoning_content"] = reason
	}
	s.writeEvent(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": d, "finish_reason": nil}},
	})
	return nil
}

// finish 是正常收尾：finish_reason + [DONE]。**只在没有错误时调用**。
func (s *sseWriter) finish() {
	s.start()
	_ = s.delta("", "") // 空流兜底：至少给一个 role 帧
	s.writeEvent(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	s.flush()
}

// fail 是错误出口：只写一帧 error，**不写 [DONE]**（截断/业务错误不得伪装正常收尾）。
func (s *sseWriter) fail(code, msg string) {
	s.start()
	s.writeEvent(map[string]any{"error": map[string]any{
		"message": msg, "type": "upstream_error", "code": code,
	}})
}

// Stream 把上游 SSE 转成 OpenAI 形状下发，返回 usage（上游未上报 ⇒ nil）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	sw := newSSEWriter(w, model)
	var textF, reasonF citationFilter
	_, frames, err := consumeTurn(r, func(text, reason string) error {
		return sw.delta(textF.Write(text), reasonF.Write(reason))
	})
	if err != nil {
		pe := asProviderError(err)
		code := "upstream_error"
		var te *truncatedErr
		if errors.As(err, &te) {
			code = "upstream_truncated"
		}
		sw.fail(code, pe.Msg)
		return nil, provider.AsClassified(pe)
	}
	if frames == 0 {
		pe := upstreamErr("上游未产出任何内容（无语义帧）")
		sw.fail("upstream_empty", pe.Msg)
		return nil, provider.AsClassified(pe)
	}
	if tail := textF.Flush(); tail != "" {
		_ = sw.delta(tail, "")
	}
	if tail := reasonF.Flush(); tail != "" {
		_ = sw.delta("", tail)
	}
	sw.finish()
	return nil, nil
}

// Aggregate 走同一条解析链，产出一条 OpenAI Chat completion 响应体。
//
// 内容口径必须与 Stream **完全一致**（同一份解析 + 同一份引用剥离），
// 否则「同一个问题流式与非流式答得不一样」。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	var textF, reasonF citationFilter
	var text, reason string
	_, frames, err := consumeTurn(r, func(t, rs string) error {
		text += textF.Write(t)
		reason += reasonF.Write(rs)
		return nil
	})
	if err != nil {
		return nil, provider.AsClassified(asProviderError(err))
	}
	text += textF.Flush()
	reason += reasonF.Flush()
	if frames == 0 && text == "" && reason == "" {
		return nil, provider.AsClassified(upstreamErr("上游未产出任何内容（无语义帧）"))
	}
	msg := map[string]any{"role": "assistant", "content": text}
	if reason != "" {
		msg["reasoning_content"] = reason
	}
	return map[string]any{
		"id":      "chatcmpl-" + newUUID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "stop"}},
		// 刻意不给 usage：上游不报 token 数，编 0 会让面板把「未上报」显示成 0。
	}, nil
}

// upstreamErr 统一「上游错误」形状：502 + 不可换号分类（唯一的构造点）。
func upstreamErr(msg string) *provider.Error {
	return &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusBadGateway, Msg: msg}
}

// asProviderError 保证错误一定是 *provider.Error（截断/形状失败都已类型化）。
func asProviderError(err error) *provider.Error {
	var te *truncatedErr
	if errors.As(err, &te) {
		return upstreamErr(te.msg)
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	return upstreamErr(err.Error())
}

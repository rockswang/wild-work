package loomy

import (
	"encoding/json"
	"fmt"
	"io"

	"wild-work/internal/provider"
)

// 上游把**鉴权失效塞在 HTTP 200 的纯 JSON 体里**（`{"code":"100002","desc":"登录已失效，
// 请重新登录"}`），整段不是 SSE。通用 Stream 只认 `data:` 前缀行 ⇒ 该体被整体跳过、
// 扫描无错 ⇒ 补一个 `[DONE]` 就「正常结束」——客户端收到一条**没有任何内容的成功流**
// （表现为「回复突然断了」），网关还把它记成成功（`NoteSuccess` + ledger `pt=0/ct=0`），
// 故障在客户端与网关两侧都不可见（2026-10-16 抓包实证）。
//
// 本渠道据此在流结束时补一次判定：整段无有效 SSE 帧且残留文本是业务错误体 ⇒ 补一帧
// error（**不补 [DONE]**，R36）并返回实现 StreamErrorClassifier 的错误，供 handler 按
// Kind() 处置（对齐 officeace 的 StreamError，R40）。
//
// ⚠️ 与 officeace 的差别：那边错误帧在**流内**（`data:{"error":...}`），原样透传即可；
// 这边是**整段非 SSE**，必须自己补帧，否则客户端拿不到任何原因。

// streamSniffLimit 流结束判定时最多留存的非 SSE 原文（业务错误体仅百余字节，8 KiB 足够；
// 设上限只为防上游异常时把大段非 SSE 文本驻留内存）。
const streamSniffLimit = 8 << 10

// StreamError 是「上游以 HTTP 200 + 非 SSE 业务错误体返回」的流内错误，
// 实现 provider.StreamErrorClassifier。
type StreamError struct {
	Status int
	Body   string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("upstream business error (http %d): %s", e.Status, truncate(e.Body, 300))
}

// Kind 复用与 HTTP 路径同一套 Classify —— 业务码优先于状态码（此处 status 恒为 200）。
func (e *StreamError) Kind() provider.ErrKind { return Classify(e.Status, e.Body) }

var _ provider.StreamErrorClassifier = (*StreamError)(nil)

// streamErrorCode 给出出口 error 帧的 code：登录态失效单独给码，便于客户端/日志区分。
func streamErrorCode(k provider.ErrKind) string {
	if k == provider.ErrSessionDead {
		return "upstream_session_dead"
	}
	return "upstream_business_error"
}

// writeStreamErrorFrame 把上游业务错误体写成一帧 SSE error（**不补 [DONE]**，R36）。
//
// 客户端据此能直接看到上游原文（如「登录已失效，请重新登录」），而不是一条空流——
// 这正是本修法的目的：把「静默断流」换成「带原因的失败」。
func writeStreamErrorFrame(w io.Writer, body string) {
	msg := truncate(body, 300)
	code := streamErrorCode(Classify(200, body))
	frame, merr := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "upstream_error",
		"code":    code,
	}})
	if merr != nil {
		// 序列化失败理论上不可能（纯字符串 map）；退化成固定帧，仍保证客户端能收尾。
		frame = []byte(`{"error":{"message":"upstream business error","type":"upstream_error","code":"upstream_business_error"}}`)
	}
	_, _ = w.Write(append(append([]byte("data: "), frame...), []byte("\n\n")...))
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
}

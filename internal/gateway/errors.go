// errors.go 协议错误响应形状 + 共用 IO 工具。
//
// 三接口的错误体形状互不兼容，必须按协议区分，否则客户端解析失败：
//   - OpenAI（Chat / Responses）: {"error":{"message","type","code"}}
//   - Anthropic                : {"type":"error","error":{"type","message"}}
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// writeOpenAIError 写 OpenAI 形状错误（Chat Completions 与 Responses 通用）。
// Responses 客户端同样接受 {"error":{...}} 形状。
func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error", "code": code},
	})
}

// writeAnthropicError 写 Anthropic 形状错误。
// typ 取 invalid_request_error / authentication_error / api_error 等。
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"message":"marshal response failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// readLimited 读取请求体（上限与内层共用 server.MaxRequestBody，issue #30）。
// 超限回 413 而非静默截断——截断后的 JSON 解析失败只会报出误导性的 invalid JSON。
func readLimited(r *http.Request, limit int64) ([]byte, error) {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("request body exceeds limit of %d bytes; please reduce conversation context", limit)
	}
	return raw, nil
}

// sseHeader 设置 SSE 响应头并返回 flush 函数（不可 flush 时返回空操作）。
func sseHeader(w http.ResponseWriter) func() {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	if fl == nil {
		return func() {}
	}
	return fl.Flush
}

// writeSSEEvent 写带 event: 行的 SSE（Anthropic 与 Responses 均使用具名事件）。
func writeSSEEvent(w http.ResponseWriter, flush func(), event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: " + event + "\ndata: " + string(raw) + "\n\n")); err != nil {
		return err
	}
	flush()
	return nil
}

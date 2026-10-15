// 模型降级标注的兼容层透传回归（PR #106）：内层在 WriteHeader 前设 X-Model-Fallback，
// /v1/responses 与 /v1/messages 的响应体由本层重建（model 回填客户端请求名），
// 该头是降级唯一的透出通道——去掉 callChat 的转写，四个用例全失败。
package gateway

import (
	"net/http"
	"testing"
)

const fbHeader = "workbuddy/glm-5.2 -> workbuddy/deepseek-v4"

// fbInner 假内层：先置降级头，再按流式/非流式回一轮正常响应。
func fbInner(stream bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Model-Fallback", fbHeader)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","model":"workbuddy/deepseek-v4","object":"chat.completion",` +
				`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"workbuddy/deepseek-v4","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"workbuddy/deepseek-v4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
}

func TestFallbackHeaderRelayedThroughGateway(t *testing.T) {
	reqBody := func(stream bool) string {
		s := "false"
		if stream {
			s = "true"
		}
		// 同时带 Responses(input) 与 Messages(messages) 两种形态的字段，两个端点共用
		return `{"model":"workbuddy/glm-5.2","stream":` + s + `,"max_tokens":64,"input":"hi","messages":[{"role":"user","content":"hi"}]}`
	}
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			_, mux := newTestGateway(fbInner(stream))
			rec := post(mux, path, reqBody(stream))
			if got := rec.Header().Get("X-Model-Fallback"); got != fbHeader {
				t.Errorf("%s stream=%v X-Model-Fallback = %q, want %q（status=%d body=%s）",
					path, stream, got, fbHeader, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestFallbackHeaderAbsentWhenNoFallback 未降级时不得凭空造出该头。
func TestFallbackHeaderAbsentWhenNoFallback(t *testing.T) {
	_, mux := newTestGateway(chatFixtureHandler(`{"id":"c1","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	rec := post(mux, "/v1/responses", `{"model":"workbuddy/glm-5.2","input":"hi"}`)
	if rec.Header().Get("X-Model-Fallback") != "" {
		t.Fatal("无降级时不应出现 X-Model-Fallback")
	}
}

package raccoon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// TestHeaderTimeoutAtLeast180s 守门：等响应头的上限不得退回 60s（R118）。
//
// 背景：小浣熊 LLM 网关是缓冲式的 —— 即便 stream:true 也要等深度思考跑到一定阶段
// 才回首字节，本机实测同模型极短请求 TTFB 64.9s / 102.9s / 95.0s。旧 60s 让正常
// 请求被稳定判死（日志 235 次 `timeout awaiting response headers`，每次精确 60.0s）。
// 因此本测试断言的是**下限**：任何人把常量调小都会被这两条断言拦下。
func TestHeaderTimeoutAtLeast180s(t *testing.T) {
	cases := []struct {
		name string
		c    *Client
		want time.Duration
	}{
		{"默认", New(), minHeaderTimeout},
		{"注入 120s 不压低响应头", NewWithTimeout(120 * time.Second), minHeaderTimeout},
		{"注入 30s 不压低响应头", NewWithTimeout(30 * time.Second), minHeaderTimeout},
		{"注入 600s 跟随放宽", NewWithTimeout(600 * time.Second), 600 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, cl := range map[string]*http.Client{"HTTP": tc.c.HTTP, "StreamHTTP": tc.c.StreamHTTP} {
				tr, ok := cl.Transport.(*http.Transport)
				if !ok || tr == nil {
					t.Fatalf("%s.Transport = %T，应为 *http.Transport", name, cl.Transport)
				}
				if want := tc.want; tr.ResponseHeaderTimeout != want {
					t.Fatalf("%s.ResponseHeaderTimeout = %v，want %v", name, tr.ResponseHeaderTimeout, want)
				}
				if tr.ResponseHeaderTimeout < minHeaderTimeout {
					t.Fatalf("R118：等响应头 %v 低于硬下限 %v", tr.ResponseHeaderTimeout, minHeaderTimeout)
				}
			}
		})
	}
}

// TestStreamHTTPNoTotalTimeout 流式 client 仍不得有整请求上限（R35 不变）。
func TestStreamHTTPNoTotalTimeout(t *testing.T) {
	if c := NewWithTimeout(600 * time.Second); c.StreamHTTP.Timeout != 0 {
		t.Fatalf("StreamHTTP.Timeout = %v，必须为 0", c.StreamHTTP.Timeout)
	}
}

// TestSlowHeaderStillSucceeds 端到端：上游迟回首字节不应被判死。
//
// 用 600ms 延迟模拟「思考中、还没首字节」的形态（真机是 60~100s，测试里按比例缩小）。
// 断言的是**行为**而非常量：若把 ResponseHeaderTimeout 改回 60s 这类小值，
// 用 600ms 仍会通过 —— 所以真正的护栏是上一条测试，本条防「改动把路径写坏」。
func TestSlowHeaderStillSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	a := &auth.Auth{AccessToken: "dummy"}

	rc, status, body, err := c.ChatStream(a, []byte(`{"model":"`+DefaultModel+`","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream: %v (status=%d body=%s)", err, status, body)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("status = %d，want 200（body=%s）", status, body)
	}
}

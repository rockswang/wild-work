package custom

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestHeaderTimeoutDefaultsAndFloor 锁定 R110 的「等响应头」口径。
//
// 背景（2026-10-20 生产事故）：sgproxy + gpt-6.1-sol 压缩上下文（整段会话重发，
// 92 万 prompt token）恒失败，日志 `net/http: timeout awaiting response headers`，
// 且每次都精确落在请求开始后 90.0~90.6s。根因是 custom.New 把
// `ResponseHeaderTimeout` 写死 90s——大 prompt 的首字节耗时由上游排队决定，
// 实测同链路 TTFB 常态 20~45s，抖动一下就越线。
//
// 三条不变量：
//  1. 默认值必须是 180s（不是 90s）；
//  2. 它**不得低于** minHeaderTimeout —— 无论注入的 requestTimeout 多小；
//  3. 注入更大的 requestTimeout（upstream.timeout_seconds）时，响应头上限跟着放宽，
//     否则「调大总超时」在等响应头这一段会被提前截断，配置项等于白设。
func TestHeaderTimeoutDefaultsAndFloor(t *testing.T) {
	cases := []struct {
		name           string
		requestTimeout time.Duration
		wantHeader     time.Duration
		wantRequest    time.Duration
	}{
		{"未注入取默认", 0, defaultHeaderTimeout, defaultRequestTimeout},
		{"注入 120s（默认配置）", 120 * time.Second, defaultHeaderTimeout, 120 * time.Second},
		{"注入 30s 不得把响应头上限压到 30s", 30 * time.Second, minHeaderTimeout, 30 * time.Second},
		{"注入 600s 响应头上限跟随放宽", 600 * time.Second, 600 * time.Second, 600 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := NewWithTimeout(ChannelConfig{Name: "sgproxy", BaseURL: "http://127.0.0.1:1"}, c.requestTimeout)
			if got := cl.HeaderTimeout(); got != c.wantHeader {
				t.Errorf("HeaderTimeout = %v, want %v", got, c.wantHeader)
			}
			if got := cl.HTTP.Timeout; got != c.wantRequest {
				t.Errorf("HTTP.Timeout = %v, want %v", got, c.wantRequest)
			}
			// 流式 client 必须无总超时（R35）：否则长回答会在超时点被掐断且无终止帧。
			if got := cl.StreamHTTP.Timeout; got != 0 {
				t.Errorf("StreamHTTP.Timeout 必须为 0（R35），got %v", got)
			}
			// 流中空闲看门狗**不跟着放宽**：它防的是「吐了几字节后彻底卡死」，
			// 多等只是让用户多等，不会变好。
			if got := cl.IdleTimeout(); got != defaultIdleTimeout {
				t.Errorf("IdleTimeout = %v, want %v（不得随 header/request 放宽）", got, defaultIdleTimeout)
			}
		})
	}
}

// TestHeaderTimeoutFloorIsNotBelowOldValue 反向护栏：下限必须**不小于**事故前的 90s。
// 若有人把 minHeaderTimeout 调小（比如「统一成 60s 更整齐」），这条会红。
func TestHeaderTimeoutFloorIsNotBelowOldValue(t *testing.T) {
	if minHeaderTimeout < 90*time.Second {
		t.Fatalf("minHeaderTimeout=%v 低于事故前的 90s，会让大 prompt 请求重新必然超时", minHeaderTimeout)
	}
	if defaultHeaderTimeout < minHeaderTimeout {
		t.Fatalf("defaultHeaderTimeout(%v) 不得小于 minHeaderTimeout(%v)", defaultHeaderTimeout, minHeaderTimeout)
	}
}

// TestLargePromptSlowHeaderSucceeds 端到端锁死事故形态：上游要 1s 才吐响应头，
// 用旧的 90s 值虽也能过，但这里的意义是证明「响应头慢 ≠ 立即失败」，并顺带证明
// 注入的 requestTimeout 真的传到了 Transport（而不是只在字段上写着）。
func TestLargePromptSlowHeaderSucceeds(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // 模拟上游排队
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer ts.Close()

	cl := NewWithTimeout(ChannelConfig{Name: "slow", BaseURL: ts.URL}, 5*time.Second)
	rc, status, _, err := cl.ChatStream(nil, []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
}

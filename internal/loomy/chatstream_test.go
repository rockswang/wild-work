package loomy

// 票 01（prefactor）回归：对话入口抽成「单次请求函数」后**可见行为逐字不变**，
// 且鉴权头构造支持显式传入 traceparent（供后续重试循环在多次尝试间复用同一个）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
)

// TestApplyAuthTraceExplicitAndGenerated 锁定新入口的两条语义：
// 显式传入的 traceparent 原样落到头上（多次尝试可复用同一个），
// 不传时仍现生成（既有调用点行为不变）。
func TestApplyAuthTraceExplicitAndGenerated(t *testing.T) {
	a := &auth.Auth{AccessToken: "tok-123"}

	const tp = "00-11111111111111111111111111111111-2222222222222222-01"
	req := httptest.NewRequest(http.MethodPost, "http://x/", nil)
	applyAuthTrace(req, a, tp)
	if got := req.Header.Get("traceparent"); got != tp {
		t.Fatalf("显式 traceparent 未被采用：got %q, want %q", got, tp)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("token"); got != "tok-123" {
		t.Fatalf("token = %q", got)
	}

	// 不传（既有入口 applyAuth）= 现生成，且两次不同。
	req2 := httptest.NewRequest(http.MethodPost, "http://x/", nil)
	applyAuth(req2, a)
	gen := req2.Header.Get("traceparent")
	if gen == "" || gen == tp {
		t.Fatalf("空 traceparent 应现生成，got %q", gen)
	}
	if parts := strings.Split(gen, "-"); len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Fatalf("生成的 traceparent 形状不对: %q", gen)
	}
	req3 := httptest.NewRequest(http.MethodPost, "http://x/", nil)
	applyAuth(req3, a)
	if req3.Header.Get("traceparent") == gen {
		t.Fatal("空 traceparent 每次都应现生成（不得缓存复用）")
	}
}

// TestChatStreamContractUnchanged 锁定抽函数后的对外契约（httptest 假上游）：
// 200 → 非 nil 流 + 200；502 → nil 流 + 502 + 响应体原文；且**每个请求只发一次**
// （本票是 prefactor，不得顺带引入重试）。
func TestChatStreamContractUnchanged(t *testing.T) {
	t.Run("200 返回可用流", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			if r.Header.Get("Authorization") != "Bearer tok-123" || r.Header.Get("token") != "tok-123" {
				t.Errorf("鉴权头缺失/不对: Authorization=%q token=%q", r.Header.Get("Authorization"), r.Header.Get("token"))
			}
			if r.Header.Get("traceparent") == "" {
				t.Error("traceparent 缺失（上游依赖它做链路追踪）")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
		}))
		defer srv.Close()

		c := New()
		c.Base = srv.URL
		rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "tok-123"}, []byte(`{"model":"`+DefaultModel+`","messages":[]}`))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if status != http.StatusOK || body != nil {
			t.Fatalf("status=%d body=%q, want 200 + nil body", status, body)
		}
		if rc == nil {
			t.Fatal("200 必须返回非 nil 流")
		}
		raw, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !strings.Contains(string(raw), "choices") {
			t.Fatalf("流内容丢失: %q", raw)
		}
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Fatalf("尝试次数 = %d, want 1（prefactor 不引入重试）", got)
		}
	})

	t.Run("502 原样透传且只发一次", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"bad gateway"}`))
		}))
		defer srv.Close()

		c := New()
		c.Base = srv.URL
		rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "tok-123"}, []byte(`{"model":"`+DefaultModel+`","messages":[]}`))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if rc != nil {
			t.Fatal("非 2xx 必须返回 nil 流")
		}
		if status != http.StatusBadGateway || string(body) != `{"message":"bad gateway"}` {
			t.Fatalf("status=%d body=%q, want 502 + 上游原文", status, body)
		}
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Fatalf("尝试次数 = %d, want 1（prefactor 不引入重试）", got)
		}
	})

	t.Run("未知模型仍是本地 400 且不打上游", func(t *testing.T) {
		var attempts int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		c := New()
		c.Base = srv.URL
		rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "tok-123"}, []byte(`{"model":"nope-xyz","messages":[]}`))
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if rc != nil || status != http.StatusBadRequest || !strings.Contains(string(body), "model_not_found") {
			t.Fatalf("rc=%v status=%d body=%q, want nil/400/model_not_found", rc, status, body)
		}
		if got := atomic.LoadInt32(&attempts); got != 0 {
			t.Fatalf("本地校验失败不得发上游，实际发了 %d 次", got)
		}
	})
}

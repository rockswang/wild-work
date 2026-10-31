package raccoon

// R148：429 等失败状态**不重试**（推翻 本渠道此前的重试实现 的 4 次尝试）。
//
// 取证依据：
//   · 官方客户端日志：429 时 is_fatal=True、duration_ms≈2.5s、total_tokens=0 ⇒ 立刻放弃；
//     它自带的 1s/2s/4s 退避被首次 fatal 短路，**实际零重试**。
//   · 官方恢复靠「静默等窗口」，窗口实测为分钟级（我方 14:12 最后一击后，官方 15:15 才恢复）。
//   · 我方 2026-10-31 实测：四轮共 12 次重试尝试**全部 429、0 次成功**，
//     即重试把「1 次越界」放大成「4 次越界」，只会延长上游惩罚。
//
// 这组测试走真实 ChatStream + 假上游，观测点是「上游收到的请求次数」——只断言常量的写法
// 测不出重试有没有接上线（本仓反复踩过的假绿形态）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"wild-work/internal/auth"
)

// TestNoRetryOn429 上游 429 时只能收到 1 次请求。
func TestNoRetryOn429(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"user_model_rate_limited","message":"您的模型请求频率超限，请稍后重试"}}`))
	}))
	defer srv.Close()

	resetSessions(t)
	c := New()
	c.Base = srv.URL
	body := []byte(`{"model":"` + DefaultModel + `","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(&auth.Auth{AccessToken: "dummy"}, body)
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	mu.Lock()
	total := hits
	mu.Unlock()
	// 期望值写死 1：任何重试都会让这里变大（旧 本渠道此前的重试实现 实现下是 4）。
	if total != 1 {
		t.Fatalf("429 时上游收到 %d 次请求，want 1（不重试）", total)
	}
	if status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", status)
	}
	if len(respBody) == 0 {
		t.Error("429 应透传上游原文，实际为空")
	}
}

// TestNoRetryOn5xx 5xx 同样不重试（旧实现按官方「5xx 可重试」会打到 4 次）。
func TestNoRetryOn5xx(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"service_unavailable"}}`))
	}))
	defer srv.Close()

	resetSessions(t)
	c := New()
	c.Base = srv.URL
	body := []byte(`{"model":"` + DefaultModel + `","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "dummy"}, body)
	if err != nil {
		t.Fatalf("不该返回 error: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
	}
	mu.Lock()
	total := hits
	mu.Unlock()
	if total != 1 {
		t.Fatalf("5xx 时上游收到 %d 次请求，want 1（不重试）", total)
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
}

// TestNoRetryOnSuccess 成功路径只发 1 次且正常交出 body（确认去掉重试环没弄坏正常流程）。
func TestNoRetryOnSuccess(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	resetSessions(t)
	c := New()
	c.Base = srv.URL
	body := []byte(`{"model":"` + DefaultModel + `","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "dummy"}, body)
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if rc == nil {
		t.Fatal("成功路径必须交出 body（否则无法读流）")
	}
	if n, _ := io.Copy(io.Discard, rc); n == 0 {
		t.Error("读到的字节数为 0，body 没正常交出")
	}
	_ = rc.Close()
	mu.Lock()
	total := hits
	mu.Unlock()
	if total != 1 {
		t.Fatalf("成功时上游收到 %d 次请求，want 1", total)
	}
}

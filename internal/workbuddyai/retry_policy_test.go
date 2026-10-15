package workbuddyai

import (
	"net/http"
	"testing"
	"time"
)

// body14003 上游瞬时拥塞限流的真实响应体（2026-10-12 生产日志实测，节选判定字段）。
// 关键在结构化字段 `retry_policy.retry=true`：这是上游的协议级承诺「这个错误可以重试」。
const body14003 = `{"code":14003,"msg":"too many requests","requestId":"r1",` +
	`"retry_policy":{"retry":true},` +
	`"displayMsg":{"en":"Too many requests. Please retry later."},` +
	`"displayTips":{"en":"Wait about a minute before retry, or switch to a less busy model."}}`

// TestRetryPolicyAllowsRetry 判据取结构化 retry_policy.retry，不是英文文案。
func TestRetryPolicyAllowsRetry(t *testing.T) {
	if !retryPolicyAllowsRetry([]byte(body14003)) {
		t.Error("14003 带 retry_policy.retry=true，应判定为可重试")
	}
	// 缺 retry_policy（普通 429 / 6004 用量配额）→ 不可重试
	noPolicy := `{"code":429,"msg":"too many requests","requestId":"r1"}`
	if retryPolicyAllowsRetry([]byte(noPolicy)) {
		t.Error("没有 retry_policy 的 429 不应判定为可重试")
	}
	// retry_policy.retry=false 显式拒绝
	refused := `{"retry_policy":{"retry":false}}`
	if retryPolicyAllowsRetry([]byte(refused)) {
		t.Error("retry_policy.retry=false 不应判定为可重试")
	}
	// 非 JSON / 空体不得 panic，一律 false
	for _, b := range []string{"", "<html>502</html>", "{", "null"} {
		if retryPolicyAllowsRetry([]byte(b)) {
			t.Errorf("非信封体 %q 不应判定为可重试", b)
		}
	}
}

// TestRetryDecisionMatrix 单点决策：可重试限流走独立预算，其余维持原语义。
func TestRetryDecisionMatrix(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		err      error
		want     bool
		wantRate bool
	}{
		{"14003 显式可重试", 429, body14003, nil, true, true},
		{"429 无 policy", 429, `{"code":429,"msg":"nope"}`, nil, false, false},
		{"6004 用量配额", 429, `{"code":6004,"msg":"usage exceeds frequency limit"}`, nil, false, false},
		{"502", 502, `<html>502</html>`, nil, true, false},
		{"400", 400, `{"code":400,"msg":"nope"}`, nil, false, false},
		{"401", 401, `{"code":401}`, nil, false, false},
	}
	for _, c := range cases {
		got, gotRate := retryDecision(c.status, []byte(c.body), c.err)
		if got != c.want || gotRate != c.wantRate {
			t.Errorf("%s: retryDecision = (%v,%v), want (%v,%v)", c.name, got, gotRate, c.want, c.wantRate)
		}
	}
}

// withFastRateLimitBackoff 把 20s 的限流退避临时缩短，返回恢复函数。
func withFastRateLimitBackoff(t *testing.T) {
	t.Helper()
	old := rateLimitBackoff
	rateLimitBackoff = 10 * time.Millisecond
	t.Cleanup(func() { rateLimitBackoff = old })
}

// TestChatStreamRetriesDeclaredRetryable429 上游自己说可以重试的 429，
// 必须在请求内重试一次再定论——否则一次瞬时拥塞会被上层的罚号放大成渠道级 503。
func TestChatStreamRetriesDeclaredRetryable429(t *testing.T) {
	withFastRateLimitBackoff(t)

	var calls int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		calls = n
		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(body14003))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, status, _, err := c.ChatStream(testAuth(), []byte(`{"model":"hy3","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != 200 || rc == nil {
		t.Fatalf("want 200 after retry, got status=%d rc=%v", status, rc)
	}
	rc.Close()
	if calls != 2 {
		t.Fatalf("expect 2 attempts (1 x 429-retryable + 1 ok), got %d", calls)
	}
}

// TestChatStreamRateLimitBudgetIsCapped 预算收紧：持续返回可重试 429 时只重试一次，
// 绝不与 5xx 的重试额度混用，避免把限流刷成刷屏（原注释担心的风控风险）。
func TestChatStreamRateLimitBudgetIsCapped(t *testing.T) {
	withFastRateLimitBackoff(t)

	var calls int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		calls = n
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body14003))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	_, status, body, err := c.ChatStream(testAuth(), []byte(`{"model":"hy3","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("want 429 passthrough after budget exhausted, got %d", status)
	}
	if len(body) == 0 {
		t.Fatal("重试耗尽后仍应把上游原文回给上层分类/透传，body 为空")
	}
	if calls != rateLimitMaxAttempts {
		t.Fatalf("want %d attempts (1 + 1 retry), got %d", rateLimitMaxAttempts, calls)
	}
}

// TestChatStreamNoRetry429WithoutPolicy 没有 retry_policy 的 429 维持老规矩：不重试。
// 这是与上一个用例的边界守门——不能因为「支持可重试 429」就把所有 429 都重试。
func TestChatStreamNoRetry429WithoutPolicy(t *testing.T) {
	var calls int
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		calls = n
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":429,"msg":"too many requests","requestId":"r1"}`))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	_, status, _, err := c.ChatStream(testAuth(), []byte(`{"model":"hy3","messages":[]}`))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", status)
	}
	if calls != 1 {
		t.Errorf("无 retry_policy 的 429 不应重试，但请求了 %d 次", calls)
	}
}

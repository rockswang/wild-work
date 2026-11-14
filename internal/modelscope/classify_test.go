package modelscope

import (
	"net/http"
	"testing"

	"wild-work/internal/provider"
)

// TestClassifyOrderRateLimitFirst 限流判定必须在**所有**基于 body 的 marker 之前
// （不变量 15 / R111）：限流报文常带 `quota` / `model not found` 之类的字样，
// 顺序一颠倒就会把「等一下再试」误判成「模型不存在」甚至硬冷却。
//
// 本渠道没有余额/权益概念（credits_na，D5），所以也不可能出现 ErrHardCredit——
// 带 `quota exceeded` 的 429 只能归 ErrSoftRate。
func TestClassifyOrderRateLimitFirst(t *testing.T) {
	c := New(provider.ModelScope)
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"429 带 quota", 429, `{"error":{"message":"quota exceeded for today","code":"rate_limit_exceeded"}}`, provider.ErrSoftRate},
		{"429 带 model not found", 429, `{"error":{"message":"model not found, try later"}}`, provider.ErrSoftRate},
		{"429 空体", 429, ``, provider.ErrSoftRate},
		// 反向护栏：marker 只在不触发 429/401 时才起作用。
		{"404 模型不存在", 404, `{"error":{"message":"model not found"}}`, provider.ErrNotFound},
		{"400 带 model not found marker", 400, `{"error":{"message":"model not found: Qwen/ghost"}}`, provider.ErrNotFound},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("%s: Classify(%d, %s) = %s，期望 %s", tc.name, tc.status, tc.body, got, tc.want)
		}
	}
	// 任何形态都不得出现「硬冷却」：本渠道无余额端点（D5），编造一个只会锁号 12h。
	if got := c.Classify(429, `{"error":{"message":"quota exceeded"}}`); got == provider.ErrHardCredit {
		t.Fatal("429 + quota 被判成 hard_credit（把限流误判成余额耗尽）")
	}
}

// TestClassifySessionDeadOn401 401/403 是凭据失效（R76）：归 ErrSessionDead 走
// 可自愈的标记（面板徽标 + 重新粘贴入口），**不得**归 ErrClient（会被当请求级问题
// 静默透传，用户看不到「要换 token」）也不得归任何会 Disable 账号的类别。
func TestClassifySessionDeadOn401(t *testing.T) {
	c := New(provider.ModelScope)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if got := c.Classify(status, `{"error":{"message":"invalid token"}}`); got != provider.ErrSessionDead {
			t.Errorf("Classify(%d) = %s，期望 session_dead", status, got)
		}
		// 401 优先于 body marker：token 失效时上游可能同时说「模型不存在」，
		// 那不代表模型没了，只代表我们没资格问。
		if got := c.Classify(status, `{"error":{"message":"model not found"}}`); got != provider.ErrSessionDead {
			t.Errorf("Classify(%d, model not found) = %s，期望 session_dead（401/403 优先）", status, got)
		}
	}
}

// TestClassifyRequestLevelAndServer 请求级/上游级错误的归属：
//   - 400/422 = ErrBadParams（请求形态问题：不罚号、不轮转、原文透传）；
//   - 5xx = ErrServer（不罚号、不换号，直接透传）；
//   - 其余 4xx = ErrClient 兜底。
func TestClassifyRequestLevelAndServer(t *testing.T) {
	c := New(provider.ModelScope)
	cases := []struct {
		status int
		want   provider.ErrKind
	}{
		{400, provider.ErrBadParams},
		{422, provider.ErrBadParams},
		{404, provider.ErrNotFound},
		{413, provider.ErrClient},
		{500, provider.ErrServer},
		{503, provider.ErrServer},
		{200, provider.ErrNone},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, ""); got != tc.want {
			t.Errorf("Classify(%d) = %s，期望 %s", tc.status, got, tc.want)
		}
	}
}

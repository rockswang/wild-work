// hard_cooldown_window_test.go 回归测试：handler 的硬冷却走「发奖窗口」策略。
//
// 锁定两件事：
//  1. 注入 HardCooldownFor 后，402 冷却时长来自钩子而非固定 HardCooldown
//     （2026-10-17 现场：qwenwork 19:07 撞 402 被锁 12h 到次日 07:07，
//     而每日额度 00:20 就到账，白锁 7 小时）；
//  2. 未注入钩子时行为与旧版完全一致（完整 HardCooldown）。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// payRequiredUpstream 假上游：一律返回 402（权益不足），用于触发硬冷却。
// 命名带 payRequired 前缀以区别于 model_fallback_14018_test.go 的 hardCreditUpstream
// （后者按 429+14018 触发 hard_credit，两者语义不同，合并 v2.8.2 时曾撞名）。
type payRequiredUpstream struct{}

func (payRequiredUpstream) RefreshToken(*auth.Auth) error { return nil }
func (payRequiredUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, http.StatusPaymentRequired, []byte(`{"code":"402","message":"Error in upstream response"}`), nil
}
func (payRequiredUpstream) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (payRequiredUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (payRequiredUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (payRequiredUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (payRequiredUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (payRequiredUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (payRequiredUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return nil, nil
}
func (payRequiredUpstream) Classify(status int, _ string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	return provider.ErrClient
}

// newHardCooldownHandler 构造一个双账号渠道 + 402 上游的 handler。
// hardFor 为 nil 时模拟旧行为（固定 HardCooldown）。
func newHardCooldownHandler(t *testing.T, hardFor func(time.Time) time.Duration) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-A", ExpiresAt: 4102444800, UID: "uA"})
	p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-B", ExpiresAt: 4102444800, UID: "uB"})
	p.SetCreditDetail("uA", 1000, 0, 0)
	p.SetCreditDetail("uB", 900, 0, 0)
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.GLM: {Kind: provider.GLM, Pool: p, Upstream: payRequiredUpstream{},
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		HardCooldown:    12 * time.Hour,
		SoftCooldown:    time.Minute,
		ErrThreshold:    3,
		ErrCooldown:     time.Minute,
		MaxRotate:       1, // 只打一个账号，便于断言该号的冷却时长
		HardCooldownFor: hardFor,
	})
	return h, p
}

// TestHardCooldownUsesWindowHook 注入钩子后，冷却时长必须来自钩子（5h23m 那种
// 截断值），而不是固定 12h。
func TestHardCooldownUsesWindowHook(t *testing.T) {
	const window = 5*time.Hour + 23*time.Minute
	h, p := newHardCooldownHandler(t, func(time.Time) time.Duration { return window })

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var cooled *pool.Status
	for _, s := range p.List() {
		st, _ := p.Status(s.UID)
		if st.Cooling {
			cooled = &st
		}
	}
	if cooled == nil {
		t.Fatal("402 后应有账号进入冷却")
	}
	got := time.Until(cooled.Until)
	// 允许少量执行耗时误差（窗口值本身不是 12h，判据用区间而非精确值）
	if got > window+time.Minute || got < window-time.Minute {
		t.Fatalf("冷却时长 = %v，期望约 %v（来自 HardCooldownFor 钩子）", got, window)
	}
	if got >= 12*time.Hour {
		t.Fatalf("不应退回固定 12h，实际 %v", got)
	}
}

// TestHardCooldownFallsBackWithoutHook 未注入钩子时保持旧行为（完整 12h）。
func TestHardCooldownFallsBackWithoutHook(t *testing.T) {
	h, p := newHardCooldownHandler(t, nil)

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var cooled *pool.Status
	for _, s := range p.List() {
		st, _ := p.Status(s.UID)
		if st.Cooling {
			cooled = &st
		}
	}
	if cooled == nil {
		t.Fatal("402 后应有账号进入冷却")
	}
	if got := time.Until(cooled.Until); got < 11*time.Hour {
		t.Fatalf("未注入钩子时应冷却约 12h，实际 %v", got)
	}
}

// TestHardCooldownIgnoresNonPositiveHook 钩子返回非正值时必须回退固定值，
// 防止「窗口算成 0/负数」把账号当场解冻（等于不冷却、每次请求都撞 402）。
func TestHardCooldownIgnoresNonPositiveHook(t *testing.T) {
	h, p := newHardCooldownHandler(t, func(time.Time) time.Duration { return 0 })

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var cooled *pool.Status
	for _, s := range p.List() {
		st, _ := p.Status(s.UID)
		if st.Cooling {
			cooled = &st
		}
	}
	if cooled == nil {
		t.Fatal("402 后应有账号进入冷却（钩子非正值不得导致不冷却）")
	}
	if got := time.Until(cooled.Until); got < 11*time.Hour {
		t.Fatalf("非正钩子值应回退 12h，实际 %v", got)
	}
}

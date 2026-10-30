package server

import (
	"net/http"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// 本文件锁定 R146：粘性路由不得把「今天有额度到期」的账号饿死在 50 次阈值之后；
// 但让位**不是一次性的**——切到临期账号后，粘性必须重新钉在它上面（会话粘性不变）。

const stickyTestKey = "x-"

func newStickyTestHandler(t *testing.T) *Handler {
	t.Helper()
	return newRotateTestHandler(t, &kindedUpstream{status: http.StatusOK}, stickyTestKey)
}

func pinSticky(t *testing.T, h *Handler, uid string) {
	t.Helper()
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(provider.GLM)] = &stickyEntry{uid: uid, maxReqs: 50}
	h.stickyMu.Unlock()
}

// TestStickyYieldsToAccountExpiringToday 粘性账号今天无到期、备选今有到期 → 让位并改钉备选。
func TestStickyYieldsToAccountExpiringToday(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, now.Add(24*time.Hour)) // 明天到期
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, now.Add(2*time.Hour))   // 今天到期
	pinSticky(t, h, stickyTestKey+"A")

	got := h.pickWithSticky(rt, "m")
	if got == nil || got.UID != stickyTestKey+"B" {
		t.Fatalf("应让位给今天到期的 B，实际 %+v", got)
	}
	e := h.sticky[h.stickyKey(provider.GLM)]
	if e == nil || e.uid != stickyTestKey+"B" {
		t.Fatalf("让位后粘性应改钉 B，实际 %+v", e)
	}
}

// TestStickyKeepsNewAccountAcrossRequests 让位后，后续请求必须继续走临期账号（粘性保持）。
func TestStickyKeepsNewAccountAcrossRequests(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, now.Add(24*time.Hour))
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, now.Add(2*time.Hour))
	pinSticky(t, h, stickyTestKey+"A")

	if got := h.pickWithSticky(rt, "m"); got == nil || got.UID != stickyTestKey+"B" {
		t.Fatalf("首次应让位到 B，实际 %+v", got)
	}
	for i := 0; i < 3; i++ {
		got := h.pickWithSticky(rt, "m")
		if got == nil || got.UID != stickyTestKey+"B" {
			t.Fatalf("第 %d 次后续请求应继续粘在 B，实际 %+v", i+2, got)
		}
	}
	if e := h.sticky[h.stickyKey(provider.GLM)]; e == nil || e.uid != stickyTestKey+"B" {
		t.Fatalf("粘性记录应持续指向 B，实际 %+v", e)
	}
}

// TestStickyHoldsWhenBothExpireToday 两者都今天到期 → 保持粘性（避免请求间抖动）。
func TestStickyHoldsWhenBothExpireToday(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, now.Add(1*time.Hour))
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, now.Add(3*time.Hour))
	pinSticky(t, h, stickyTestKey+"A")

	if got := h.pickWithSticky(rt, "m"); got == nil || got.UID != stickyTestKey+"A" {
		t.Fatalf("两者都今天到期时应保持粘性 A，实际 %+v", got)
	}
}

// TestStickyHoldsWhenNobodyExpiresToday 无人今天到期 → 保持粘性。
func TestStickyHoldsWhenNobodyExpiresToday(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, now.Add(48*time.Hour))
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, now.Add(24*time.Hour))
	pinSticky(t, h, stickyTestKey+"A")

	if got := h.pickWithSticky(rt, "m"); got == nil || got.UID != stickyTestKey+"A" {
		t.Fatalf("无人今天到期时应保持粘性 A，实际 %+v", got)
	}
}

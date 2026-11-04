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

// ---- 到期时刻的构造：不许写死 now+Δ（2026-11-04 修 R146 用例的墙钟脆弱点） ----
//
// 「今天到期」的判据是 R105 的桶判据：把到期时刻**先减 5 分钟零点宽限**、再按
// **UTC+8** 自然日分档，且「减完宽限还不晚于 now」直接算已失效
// （provider.expireDayOf）。于是写死 now+Δ 的数据在两个时段必然自相矛盾：
//   - now+2h：22:00 之后跑就跨到明天 ⇒「今天到期」不成立，让位用例必红；
//   - now+24h：UTC+8 凌晨 00:00~00:05 跑会落回**今天**桶（宽限所致）
//     ⇒「明天到期」不成立，反向用例必红。
// 白天跑永远绿，所以这种脆弱点只在夜里/凌晨暴露。
//
// 修法不复制那两个未导出常量（宽限与 expireLoc），而是拿**桶判据本身**当 oracle
// 反推：能过桶判据的时刻，一定既没过期、又在目标桶内。

// bucketExpiry 返回自 now 起第一个落在 want 桶内的到期时刻。
//
// 步长自适应：前 15 分钟按 1 秒细扫——「今天」桶的可行窗口起点恰好是
// now+零点宽限（5 分钟），细扫必定命中；15 分钟内没命中说明目标窗口在下一个
// 自然日（宽约 24 小时），改 1 分钟步长即可，总量仍有界。
func bucketExpiry(t *testing.T, now time.Time, want int) time.Time {
	t.Helper()
	for d := time.Second; d <= 25*time.Hour; {
		if x := now.Add(d); provider.ExpireBucketOf(x.Format(time.RFC3339), now) == want {
			return x
		}
		if d < 15*time.Minute {
			d += time.Second
		} else {
			d += time.Minute
		}
	}
	if want == provider.BucketToday {
		// 到期时刻在判据里只有秒级精度（expiresToday 走 Unix()），而「今天」的可行
		// 窗口 = (now+宽限, 次日 00:00+宽限)：UTC+8 当天最后一秒里它是空的。
		t.Skipf("UTC+8 当日只剩不到 1 秒，无法构造「仍有效且今天到期」的时刻（now=%s）",
			now.Format(time.RFC3339))
	}
	t.Fatalf("构造不出桶 %d 的到期时刻（now=%s）", want, now.Format(time.RFC3339))
	return time.Time{}
}

// todayExpiry 尽量取 now+d；跨出今天桶时退回今天桶里第一个可行时刻。
func todayExpiry(t *testing.T, now time.Time, d time.Duration) time.Time {
	t.Helper()
	if x := now.Add(d); provider.ExpireBucketOf(x.Format(time.RFC3339), now) == provider.BucketToday {
		return x
	}
	return bucketExpiry(t, now, provider.BucketToday)
}

// tomorrowExpiry 同上，但目标桶是「明天」。
func tomorrowExpiry(t *testing.T, now time.Time, d time.Duration) time.Time {
	t.Helper()
	if x := now.Add(d); provider.ExpireBucketOf(x.Format(time.RFC3339), now) == provider.BucketTomorrow {
		return x
	}
	return bucketExpiry(t, now, provider.BucketTomorrow)
}

// todayExpiryAfter 返回严格晚于 after 的一个「今天到期」时刻。给「两者都今天到期、
// 但粘性账号不是池内最优」的场景用：这样让位判定才会真的走到 R146 那条
// 「都今天到期 ⇒ 保持粘性」的守卫，而不是被 best==sticky 提前短路。
func todayExpiryAfter(t *testing.T, now, after time.Time) time.Time {
	t.Helper()
	for d := after.Sub(now) + time.Second; d <= 25*time.Hour; {
		if x := now.Add(d); provider.ExpireBucketOf(x.Format(time.RFC3339), now) == provider.BucketToday {
			return x
		}
		if d < 15*time.Minute {
			d += time.Second
		} else {
			d += time.Minute
		}
	}
	t.Skipf("UTC+8 当日已无更晚的「今天到期」时刻（now=%s after=%s）",
		now.Format(time.RFC3339), after.Format(time.RFC3339))
	return time.Time{}
}

// TestStickyExpiryDataIsClockRobust 逐分钟扫一个完整自然日，验证上面三个构造器在
// **任意**墙钟时刻都成立。这正是原用例缺的保证：老写法白天永远绿、入夜才红。
func TestStickyExpiryDataIsClockRobust(t *testing.T) {
	base := time.Date(2026, 3, 15, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	for i := 0; i < 24*60; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		for _, d := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
			x := todayExpiry(t, now, d)
			if got := provider.ExpireBucketOf(x.Format(time.RFC3339), now); got != provider.BucketToday {
				t.Fatalf("now=%s d=%s：构造出的 %s 桶=%d，期望今天",
					now.Format(time.RFC3339), d, x.Format(time.RFC3339), got)
			}
		}
		y := tomorrowExpiry(t, now, 24*time.Hour)
		if got := provider.ExpireBucketOf(y.Format(time.RFC3339), now); got != provider.BucketTomorrow {
			t.Fatalf("now=%s：构造出的 %s 桶=%d，期望明天",
				now.Format(time.RFC3339), y.Format(time.RFC3339), got)
		}
	}
}

// TestStickyYieldsToAccountExpiringToday 粘性账号今天无到期、备选今有到期 → 让位并改钉备选。
func TestStickyYieldsToAccountExpiringToday(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, tomorrowExpiry(t, now, 24*time.Hour)) // 明天到期
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, todayExpiry(t, now, 2*time.Hour))      // 今天到期
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
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, tomorrowExpiry(t, now, 24*time.Hour))
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, todayExpiry(t, now, 2*time.Hour))
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
//
// 数据刻意让 **B 更早**（池内最优是 B、不是粘性账号 A）：这样才会真的走进
// yieldForExpiringToday 的「都今天到期 ⇒ 保持粘性」守卫；若按原样让 A 更早，
// best==sticky 会先短路返回，那条守卫从未被执行、删掉它这条用例也不会红。
func TestStickyHoldsWhenBothExpireToday(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	b := todayExpiry(t, now, time.Hour)
	a := todayExpiryAfter(t, now, b)
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, a)
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, b)
	pinSticky(t, h, stickyTestKey+"A")

	if got := h.pickWithSticky(rt, "m"); got == nil || got.UID != stickyTestKey+"A" {
		t.Fatalf("两者都今天到期时应保持粘性 A，实际 %+v", got)
	}
}

// TestStickyHoldsWhenNobodyExpiresToday 无人今天到期 → 保持粘性（B 是池内最优也不让位）。
func TestStickyHoldsWhenNobodyExpiresToday(t *testing.T) {
	h := newStickyTestHandler(t)
	rt := h.cfg.Runtimes[provider.GLM]
	now := time.Now()
	rt.Pool.SetCreditDetail(stickyTestKey+"A", 1000, 0, 0, now.Add(48*time.Hour))               // 7 日桶，不是今天
	rt.Pool.SetCreditDetail(stickyTestKey+"B", 500, 0, 0, tomorrowExpiry(t, now, 24*time.Hour)) // 明天
	pinSticky(t, h, stickyTestKey+"A")

	if got := h.pickWithSticky(rt, "m"); got == nil || got.UID != stickyTestKey+"A" {
		t.Fatalf("无人今天到期时应保持粘性 A，实际 %+v", got)
	}
}

package app

// 临期展示窗口可配（R112b）的回归测试。
//
// 背景（2026-11-04 用户实测，远程实例 v2.10.0）：积分汇总里的「临期」全空，
// 而运行统计页显示好几个渠道 24 小时内到期。根因是 R112 把窗口固定成
// provider.ExpiringDays = 1（自然日「今天」），而 traework/workbuddy 的每日签到包
// 是「每天 05:47 发、次日 05:47 到期」——上午 9 点看，最近一笔永远落在「明天」
// 那一自然日 ⇒ 一天里大部分时间汇总条必然没有临期。
//
// 修法：口径恢复为可配 1/2/7（与运行统计三桶同源），但仍**不参与选号**。
// 这里锁住三件事：
//  1. SetExpiringDays 只接受 1/2/7，非法值报错且不改配置；
//  2. 改口径后 creditSnapshot 立即按新窗口重算并写回 pool（面板当场生效）；
//  3. 选号判据不受影响（earliest_expire 与窗口无关）。

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/provider"
)

// fixedItemsUpstream 返回固定条目（其余沿用 fakeUpstream 的空实现）。
type fixedItemsUpstream struct {
	fakeUpstream
	items []provider.ResourceItem
}

func (f *fixedItemsUpstream) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	f.resourceCalls.Add(1)
	var remain int64
	for _, it := range f.items {
		if it.Usable && !it.InfoOnly {
			remain += it.Remain
		}
	}
	return remain, f.items, nil
}

// nextMorningAt 返回「下一次本地时间 hh:mm」的绝对时刻（严格晚于 now）。
// 用 05:47 复刻用户现场那批每日包的到期形态。
func nextMorningAt(now time.Time, hh, mm int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// TestSetExpiringDaysValidation 面板只能选 1/2/7；写入非法值时配置不变。
func TestSetExpiringDaysValidation(t *testing.T) {
	up := &fakeUpstream{remain: 100}
	a, _ := newTestApp(t, provider.WorkBuddyAI, up, "uid-1")
	if got := a.expiringDays(); got != config.DefaultExpiringDays {
		t.Fatalf("默认口径应为 %d，实际 %d", config.DefaultExpiringDays, got)
	}
	for _, bad := range []int{0, 3, 5, 8, -1} {
		if err := a.SetExpiringDays(bad); err == nil {
			t.Fatalf("%d 天应被拒绝", bad)
		}
		if got := a.expiringDays(); got != config.DefaultExpiringDays {
			t.Fatalf("拒绝 %d 后口径不应改变，实际 %d", bad, got)
		}
	}
	for _, ok := range []int{1, 2, 7} {
		before := up.resourceCalls.Load()
		if err := a.SetExpiringDays(ok); err != nil {
			t.Fatalf("%d 天应被接受：%v", ok, err)
		}
		// SetExpiringDays 会 go RefreshAll() 立即重刷；等它落地再继续，
		// 否则 t.TempDir 清理时后台 goroutine 还握着 ledger 文件句柄（Windows）。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && up.resourceCalls.Load() == before {
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if got := a.expiringDays(); got != ok {
			t.Fatalf("设置 %d 后口径应为 %d，实际 %d", ok, ok, got)
		}
	}
}

// TestExpiringDaysDrivesSnapshotWindow 换口径后同一条目是否算临期随之变化。
//
// 「明早 05:47 失效」= 用户现场那批每日包的形态：now 为当天上午 ⇒ 按自然日归「明日」
// ⇒ 1 天窗口 = 0（这就是 bug 的表象），2 天 / 7 天窗口 = 全额。
func TestExpiringDaysDrivesSnapshotWindow(t *testing.T) {
	now := time.Now()
	exp := nextMorningAt(now, 5, 47)
	up := &fixedItemsUpstream{items: []provider.ResourceItem{
		{Name: "每日签到", Remain: 100, Usable: true, ExpireAt: exp.Format(time.RFC3339)},
	}}
	a, p := newTestApp(t, provider.WorkBuddyAI, up, "uid-1")

	if _, _, expiring1, _, err := a.creditSnapshot(a.runtime(provider.WorkBuddyAI), p.AuthByUID("uid-1")); err != nil {
		t.Fatalf("creditSnapshot: %v", err)
	} else if expiring1 != 0 {
		// 明天 05:47 属「明日」桶 ⇒ 1 天窗口（今日）不计入。
		// （若 now 恰好在今天 00:00~05:47 之间，该时刻仍是「今日」桶，故此处只在
		//  now >= 05:47 时才断言 0，避免时间敏感的假失败。）
		dayOfNow := time.Date(now.Year(), now.Month(), now.Day(), 5, 47, 0, 0, now.Location())
		if now.After(dayOfNow) && expiring1 != 0 {
			t.Fatalf("1 天窗口下「明日到期」不应计入临期，实际 %d", expiring1)
		}
	}
	if err := a.SetExpiringDays(2); err != nil {
		t.Fatal(err)
	}
	waitRefreshIdle(t, a)
	if _, _, expiring2, _, err := a.creditSnapshot(a.runtime(provider.WorkBuddyAI), p.AuthByUID("uid-1")); err != nil {
		t.Fatalf("creditSnapshot: %v", err)
	} else if expiring2 != 100 {
		t.Fatalf("2 天窗口下应计入 100，实际 %d", expiring2)
	}
	// 等 SetExpiringDays 触发的异步刷新落地，再断言（也让 TempDir 清理不被抢写）。
	waitRefreshIdle(t, a)
	// pool 快照必须同步：面板读的是 pool.Status 的 ExpiringCredits。
	if st, ok := p.Status("uid-1"); !ok || st.ExpiringCredits != 100 {
		t.Fatalf("pool 临期未同步，st=%+v", st)
	}
	if err := a.SetExpiringDays(7); err != nil {
		t.Fatal(err)
	}
	if _, _, expiring7, _, err := a.creditSnapshot(a.runtime(provider.WorkBuddyAI), p.AuthByUID("uid-1")); err != nil {
		t.Fatalf("creditSnapshot: %v", err)
	} else if expiring7 != 100 {
		t.Fatalf("7 天窗口下应计入 100（只有一个包），实际 %d", expiring7)
	}
}

// TestExpiringDaysDoesNotAffectRouting 换口径不得改变选号判据（R112 的核心不变量）。
//
// 单账号两条包：明早 05:47 到期 100 分、五天后到期 500 分。
// 无论窗口是 1 还是 7，pool 记录的「最早到期时刻」都必须是最早那条
// —— 选号只看时刻（EarliestExpire），不看窗口内的额度体量。
func TestExpiringDaysDoesNotAffectRouting(t *testing.T) {
	now := time.Now()
	soon := nextMorningAt(now, 5, 47)
	up := &fixedItemsUpstream{items: []provider.ResourceItem{
		{Name: "soon", Remain: 100, Usable: true, ExpireAt: soon.Format(time.RFC3339)},
		{Name: "later", Remain: 500, Usable: true, ExpireAt: now.Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}}
	a, p := newTestApp(t, provider.WorkBuddyAI, up, "uid-1")
	rt := a.runtime(provider.WorkBuddyAI)
	au := p.AuthByUID("uid-1")

	if _, _, _, _, err := a.creditSnapshot(rt, au); err != nil {
		t.Fatalf("creditSnapshot: %v", err)
	}
	earliestBefore, hasBefore := p.EarliestExpireOf("uid-1")
	if !hasBefore || earliestBefore.IsZero() {
		t.Fatalf("应记录最早到期时刻")
	}
	if err := a.SetExpiringDays(7); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := a.creditSnapshot(rt, au); err != nil {
		t.Fatalf("creditSnapshot: %v", err)
	}
	if got, _ := p.EarliestExpireOf("uid-1"); !got.Equal(earliestBefore) {
		t.Fatalf("换临期口径后最早到期时刻不应改变：before=%v after=%v", earliestBefore, got)
	}
}

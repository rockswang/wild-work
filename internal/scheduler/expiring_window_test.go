// expiring_window_test.go 锁住 R112b 在调度器侧的接线：
// 签到路径写回 pool 的 expiring 必须用「当前临期口径」，而不是包内常量。
//
// 为什么这条必须单独测：App.SetExpiringDays 会推给所有调度器，但**推漏了**不会有
// 任何显式失败——用户把口径改成 7 天，面板当场（RefReshAll）显示正常，
// 然后下一次自动签到用旧窗口重算并写回 pool，数字悄悄跳回 1 天的值。
package scheduler

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// expiringWindowUpstream 每条请求都回同一批条目：今晚 23:59:59 到期 100，
// 明早 05:47 到期 200（后者按自然日属「明日」⇒ 只在 2/7 天窗口里算临期）。
type expiringWindowUpstream struct {
	noCheckinUpstream
	items []provider.ResourceItem
}

func (u *expiringWindowUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 300, u.items, nil
}

func (u *expiringWindowUpstream) DailyCheckin(a *auth.Auth) error { return nil }

// TestCheckinPathUsesConfiguredExpiringWindow 窗口从 1 改到 2 后，
// 同一次签到写回 pool 的 ExpiringCredits 必须按新窗口算（多出「明日」那一笔）。
func TestCheckinPathUsesConfiguredExpiringWindow(t *testing.T) {
	now := time.Now()
	tonight := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, now.Location())
	tomorrow := tonight.Add(24 * time.Hour)
	_ = tomorrow
	items := []provider.ResourceItem{
		{Name: "今日包", Remain: 100, Usable: true, ExpireAt: tonight.Format(time.RFC3339)},
		// 明早 05:47（若 now 已过 05:47，则顺延到明天，落「明日」桶）
		{Name: "明日包", Remain: 200, Usable: true, ExpireAt: nextLocal(now, 5, 47).Format(time.RFC3339)},
	}

	p := newNoCheckinPool()
	up := &expiringWindowUpstream{items: items}
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddy", CheckinMinutes: []int{9 * 60}, KeepaliveHours: []int{}})

	if _, err := s.CheckinAccount("u1"); err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	st, _ := p.Status("u1")
	// 默认 1 天：只应计入今晚那一笔（除非 now 早于 05:47，见下方注释）。
	if st.ExpiringCredits < 100 || st.ExpiringCredits > 300 {
		t.Fatalf("1 天窗口的临期应在 [100,300] 内，实际 %d", st.ExpiringCredits)
	}

	s.SetExpiringDays(2)
	if got := s.ExpiringDays(); got != 2 {
		t.Fatalf("SetExpiringDays 未生效：%d", got)
	}
	if _, err := s.CheckinAccount("u1"); err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	st2, _ := p.Status("u1")
	if st2.ExpiringCredits != 300 {
		t.Fatalf("2 天窗口下两笔都应计入（共 300），实际 %d（签到路径仍用旧窗口）", st2.ExpiringCredits)
	}
}

// TestSchedulerExpiringDaysDefault 未配置（0）时回落 provider 默认值，
// 保证「命令行/测试里直接 scheduler.New(Config{...})」不会算出 0 天窗口。
func TestSchedulerExpiringDaysDefault(t *testing.T) {
	s := New(Config{Pool: newNoCheckinPool(), Upstream: &noCheckinUpstream{}, Name: "workbuddy", CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	if got := s.ExpiringDays(); got != provider.ExpiringDays {
		t.Fatalf("未配置时应回落到 %d，实际 %d", provider.ExpiringDays, got)
	}
	s.SetExpiringDays(7)
	if got := s.ExpiringDays(); got != 7 {
		t.Fatalf("SetExpiringDays(7) 后应为 7，实际 %d", got)
	}
}

// nextLocal 下一次本地 hh:mm（严格晚于 now）。
func nextLocal(now time.Time, hh, mm int) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), hh, mm, 0, 0, now.Location())
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

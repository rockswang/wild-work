// expiring_days_broadcast_test.go 锁住 R112b 的接线：面板改临期口径时，
// 必须**同时**推给所有调度器，否则签到路径会继续用旧窗口写 expiring，
// 下一次签到就把 pool 里的值改回去（面板数字随即跳回旧口径）。
package app

import (
	"testing"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/provider"
	"wild-work/internal/scheduler"
)

// waitRefreshIdle 等 SetExpiringDays 触发的异步 RefreshAll 收尾。
// 不等的话 t.TempDir 清理时后台 goroutine 还在写 state/ledger 文件
// （Windows 会报 "The process cannot access the file" / "directory is not empty"）。
// 直接用 App 自己的收尾信号（Close 也走同一个，见 waitRefresh）。
func waitRefreshIdle(t *testing.T, a *App) {
	t.Helper()
	a.waitRefresh(5 * time.Second)
}

// TestSetExpiringDaysBroadcastsToAllSchedulers 所有渠道的调度器都要换窗口
// ——与签到时刻不同，临期口径是**纯展示**口径，不存在「本渠道不适用」的情况。
func TestSetExpiringDaysBroadcastsToAllSchedulers(t *testing.T) {
	a, schs := newSchedApp(t)

	for _, days := range []int{2, 7, 1} {
		if err := a.SetExpiringDays(days); err != nil {
			t.Fatalf("SetExpiringDays(%d): %v", days, err)
		}
		waitRefreshIdle(t, a)
		for kind, s := range schs {
			if got := s.ExpiringDays(); got != days {
				t.Fatalf("渠道 %s 的临期窗口应为 %d，实际 %d（签到路径会写回旧口径）", kind, days, got)
			}
		}
		if got := a.expiringDays(); got != days {
			t.Fatalf("App 口径应为 %d，实际 %d", days, got)
		}
		if got := a.GetState().ExpiringDays; got != days {
			t.Fatalf("State 下发给面板的口径应为 %d，实际 %d", days, got)
		}
	}
}

// TestSetExpiringDaysRejectsWithoutSideEffect 非法值必须原样报错且不动任何状态
// ——面板会把错误弹给用户，但不该在后端留下「一半生效」的痕迹。
func TestSetExpiringDaysRejectsWithoutSideEffect(t *testing.T) {
	a, schs := newSchedApp(t)
	if err := a.SetExpiringDays(3); err == nil {
		t.Fatalf("3 天（R112 前的旧选项）必须被拒绝")
	}
	if got := a.expiringDays(); got != config.DefaultExpiringDays {
		t.Fatalf("拒绝后 App 口径应保持 %d，实际 %d", config.DefaultExpiringDays, got)
	}
	for kind, s := range schs {
		if got := s.ExpiringDays(); got != config.DefaultExpiringDays {
			t.Fatalf("拒绝后渠道 %s 的窗口应保持 %d，实际 %d", kind, config.DefaultExpiringDays, got)
		}
	}
	if got := a.GetState().ExpiringDays; got != config.DefaultExpiringDays {
		t.Fatalf("拒绝后面板仍应看到 %d，实际 %d", config.DefaultExpiringDays, got)
	}
}

// TestExpiringDaysDoesNotMoveRouting EarliestExpire 与窗口完全解耦：
// 换窗口只影响调度器写回的展示值，选号判据不受影响（R112 的核心不变量）。
func TestExpiringDaysDoesNotMoveRouting(t *testing.T) {
	a, schs := newSchedApp(t)
	rt := a.runtime(provider.WorkBuddy)
	if rt == nil || rt.Scheduler == nil {
		t.Fatalf("缺少 workbuddy 运行时/调度器")
	}
	if err := a.SetExpiringDays(7); err != nil {
		t.Fatalf("SetExpiringDays: %v", err)
	}
	waitRefreshIdle(t, a)
	if got := schs[provider.WorkBuddy].ExpiringDays(); got != 7 {
		t.Fatalf("调度器窗口应为 7，实际 %d", got)
	}
	// 调度器接口上唯一的窗口入口是 SetExpiringDays/ExpiringDays，
	// 选号走 pool 的 EarliestExpire；这里断言窗口值确实传到了，
	// 而选号路径（pool）不读它——由 pool 的单测保证排序与窗口无关。
	if _, ok := rt.Pool.Status("u-workbuddy"); !ok {
		t.Fatalf("账号应存在于 pool")
	}
	_ = scheduler.Config{} // 保持 import 使用（newSchedApp 返回 scheduler 类型）
}

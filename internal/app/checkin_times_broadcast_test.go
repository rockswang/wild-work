// checkin_times_broadcast_test.go 锁住 issue #210 的另一半：
// 面板改签到时间时，只推给「签到时刻跟随全局配置」的调度器。
//
// 旧实现无条件广播给所有调度器，于是：
//   - 显式配成 []int{} 的渠道（qwenwork/cline/comate/oczen/kilo/zcode）在用户改过一次
//     签到时间后就获得了签到时刻，从此每天空跑必然失败的签到并把面板染红（issue #210）；
//   - Qoder 双区用上游固定的 10:00 开放窗口，会被全局的 09:00/21:00 覆盖掉。
package app

import (
	"path/filepath"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/scheduler"
)

// newSchedApp 构造一个带三个调度器的最小 App：
// 跟随全局的 workbuddy、无签到任务的 cline、固定窗口的 qodercn。
func newSchedApp(t *testing.T) (*App, map[provider.Kind]*scheduler.Scheduler) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	mk := func(kind provider.Kind, schedCfg scheduler.Config) (*Runtime, *scheduler.Scheduler) {
		p := pool.New(filepath.Join(dir, "state-"+kind.String()+".json"))
		p.Add(&auth.Auth{Kind: kind.String(), UID: "u-" + kind.String(), AccessToken: "t", RefreshToken: "r"})
		schedCfg.Pool = p
		schedCfg.Upstream = &fakeUpstream{}
		schedCfg.Name = kind.String()
		s := scheduler.New(schedCfg)
		return &Runtime{Kind: kind, Pool: p, Upstream: &fakeUpstream{}, Scheduler: s}, s
	}

	wbRt, wbS := mk(provider.WorkBuddy, scheduler.Config{
		FollowGlobalCheckin: true, CheckinMinutes: []int{9 * 60, 21 * 60}, KeepaliveHours: []int{},
	})
	// 无签到任务：显式空切片（R39）。
	clRt, clS := mk(provider.Cline, scheduler.Config{CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// Qoder 双区：固定 10:00 开放窗口，不跟随全局。
	qcnRt, qcnS := mk(provider.QoderCN, scheduler.Config{CheckinMinutes: []int{10 * 60}, KeepaliveHours: []int{}})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: wbRt,
			provider.Cline:     clRt,
			provider.QoderCN:   qcnRt,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, map[provider.Kind]*scheduler.Scheduler{
		provider.WorkBuddy: wbS,
		provider.Cline:     clS,
		provider.QoderCN:   qcnS,
	}
}

// TestSetCheckinTimesBroadcastsOnlyToFollowers 只有声明跟随全局的调度器被更新。
func TestSetCheckinTimesBroadcastsOnlyToFollowers(t *testing.T) {
	a, schs := newSchedApp(t)

	if err := a.SetCheckinTimes([]string{"07:15", "20:45"}); err != nil {
		t.Fatalf("SetCheckinTimes: %v", err)
	}

	got := schs[provider.WorkBuddy].CheckinTimes()
	if len(got) != 2 || got[0] != "07:15" || got[1] != "20:45" {
		t.Fatalf("跟随全局的渠道应被更新，实际 %v", got)
	}

	// 无签到任务的渠道必须保持为空——否则它会获得签到时刻，每天空跑必然失败的签到，
	// 并把 Pool.RecordCheckin 写成 ok=false（面板红色「签到失败」，issue #210）。
	if cl := schs[provider.Cline].CheckinMinutes(); len(cl) != 0 {
		t.Fatalf("无签到任务的渠道不得被推送签到时刻，实际 %v（会每天空跑并污染面板）", cl)
	}

	// Qoder 双区：上游活动 10:00 才开放，起点被改到 07:15 会永远抢在活动创建之前。
	if qcn := schs[provider.QoderCN].CheckinTimes(); len(qcn) != 1 || qcn[0] != "10:00" {
		t.Fatalf("固定窗口的渠道不得被全局时刻覆盖，实际 %v", qcn)
	}

	// 配置本身仍要落盘（面板回显用），与调度器推送范围是两件事。
	if ct := a.checkinTimes(); len(ct) != 2 || ct[0] != "07:15" {
		t.Fatalf("配置未更新：%v", ct)
	}
}

// TestSetCheckinTimesKeepsQoderFixedWindow 反复改时间也不得把 Qoder 的固定窗口挪走。
func TestSetCheckinTimesKeepsQoderFixedWindow(t *testing.T) {
	a, schs := newSchedApp(t)
	for _, times := range [][]string{{"09:00"}, {"09:00", "21:00"}, {"23:30"}} {
		if err := a.SetCheckinTimes(times); err != nil {
			t.Fatalf("SetCheckinTimes(%v): %v", times, err)
		}
		if qcn := schs[provider.QoderCN].CheckinTimes(); len(qcn) != 1 || qcn[0] != "10:00" {
			t.Fatalf("第 %v 次更新后 Qoder 窗口变成 %v", times, qcn)
		}
	}
}

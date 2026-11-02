// no_checkin_times_test.go 锁住 issue #210 的后端半边：
// 「本渠道没有签到时刻」时，签到必然失败的上游桩不得被记成「签到失败」落盘。
//
// 现场（用户 czm1231 的 fnOS 面板）：qwenwork / kilo / oczen 卡片长期红「签到失败」，
// 但积分照常到账（日志 remain=2050）。根因是两条路径叠加——
//  1. 这些渠道的 DailyCheckin 桩恒返回「无签到活动」错误（本就没打算被调用）；
//  2. SetCheckinTimes 无条件把全局时刻广播给所有调度器，于是用户改一次签到时间
//     就给它们装上了签到任务，每天空跑一次必然失败，Pool.RecordCheckin 写 ok=false。
//
// 修法：finishCheckin 在「无签到时刻」或 ActivitiesOnly 时直接返回，不落盘不上报。
package scheduler

import (
	"errors"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// noCheckinUpstream 无签到能力的渠道桩：DailyCheckin 恒返回错误（与各渠道实现同形，
// 例如 qwenwork 的「无签到活动（每日积分服务端自动发放，无需领取）」）。
type noCheckinUpstream struct {
	provider.Upstream
	err   error
	calls int32
}

func (u *noCheckinUpstream) DailyCheckin(a *auth.Auth) error {
	u.calls++
	return u.err
}

func (u *noCheckinUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 500, []provider.ResourceItem{{Name: "每日额度", Remain: 500, Usable: true}}, nil
}

func newNoCheckinPool() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(48 * time.Hour).Unix()})
	return p
}

// TestNoCheckinTimesSkipsRecordCheckin 无签到时刻 ⇒ 失败结果不落盘（面板不会染红）。
func TestNoCheckinTimesSkipsRecordCheckin(t *testing.T) {
	p := newNoCheckinPool()
	up := &noCheckinUpstream{err: errors.New("qwenwork 无签到活动（每日积分服务端自动发放，无需领取）")}
	// 显式空切片 = 本渠道没有签到任务（R39）。
	s := New(Config{Pool: p, Upstream: up, Name: "qwenwork", CheckinMinutes: []int{}, KeepaliveHours: []int{}})

	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if res.OK {
		t.Fatalf("上游报错时结果不应是成功：%+v", res)
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if !st.LastCheckinAt.IsZero() {
		t.Fatalf("无签到时刻的渠道不得写签到时间戳（面板会渲染红色「签到失败」）：%v", st.LastCheckinAt)
	}
	if st.LastCheckinOK {
		t.Fatal("不得写 ok=true")
	}
	if st.LastCheckinMsg != "" {
		t.Fatalf("不得写签到消息：%q", st.LastCheckinMsg)
	}
	// 余额刷新仍必须发生：签到与积分刷新共用一次批量调用，跳过记录不等于跳过刷新。
	if st.Credits != 500 {
		t.Fatalf("余额应照常刷新，credits=%d", st.Credits)
	}
}

// TestHasCheckinTimesStillRecords 反向护栏：有签到时刻的渠道必须照常记录失败。
// 去掉这个用例，把 finishCheckin 改成「永不记录」也能让上一条测试通过。
func TestHasCheckinTimesStillRecords(t *testing.T) {
	p := newNoCheckinPool()
	up := &noCheckinUpstream{err: errors.New("上游 503")}
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddy", CheckinMinutes: []int{9 * 60}, KeepaliveHours: []int{}})

	if _, err := s.CheckinAccount("u1"); err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	st, _ := p.Status("u1")
	if st.LastCheckinAt.IsZero() {
		t.Fatal("有签到时刻的渠道必须记录签到时间戳")
	}
	if st.LastCheckinOK {
		t.Fatal("失败结果应记 ok=false")
	}
	if st.LastCheckinMsg == "" {
		t.Fatal("失败原因必须落盘（面板/日志要能看出为什么失败）")
	}
}

// TestActivitiesOnlySkipsRecordCheckin 既有语义不许回退：WorkBuddy 国际版（ActivitiesOnly）
// 即使配了签到时刻，也不记录/不上报。
func TestActivitiesOnlySkipsRecordCheckin(t *testing.T) {
	p := newNoCheckinPool()
	up := &noCheckinUpstream{}
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddyai",
		CheckinMinutes: []int{9 * 60}, KeepaliveHours: []int{}, ActivitiesOnly: true})

	if _, err := s.CheckinAccount("u1"); err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if st, _ := p.Status("u1"); !st.LastCheckinAt.IsZero() {
		t.Fatalf("ActivitiesOnly 渠道不得写签到时间戳：%v", st.LastCheckinAt)
	}
	if up.calls == 0 {
		t.Fatal("ActivitiesOnly 仍须调用 DailyCheckin（活跃保活）")
	}
}

// TestFollowsGlobalCheckinFlag 热更新推送判据：只有声明跟随全局的调度器才应被推送。
func TestFollowsGlobalCheckinFlag(t *testing.T) {
	follower := New(Config{Pool: newNoCheckinPool(), Upstream: &noCheckinUpstream{},
		Name: "workbuddy", FollowGlobalCheckin: true, CheckinMinutes: []int{9 * 60}, KeepaliveHours: []int{}})
	fixed := New(Config{Pool: newNoCheckinPool(), Upstream: &noCheckinUpstream{},
		Name: "qodercn", CheckinMinutes: []int{10 * 60}, KeepaliveHours: []int{}})
	empty := New(Config{Pool: newNoCheckinPool(), Upstream: &noCheckinUpstream{},
		Name: "cline", CheckinMinutes: []int{}, KeepaliveHours: []int{}})

	if !follower.FollowsGlobalCheckin() {
		t.Error("workbuddy 应跟随全局签到时刻")
	}
	// Qoder 双区用上游固定的 10:00 开放窗口，全局时刻会把它推到活动创建之前。
	if fixed.FollowsGlobalCheckin() {
		t.Error("qodercn 不应跟随全局签到时刻（固定 10:00 窗口）")
	}
	if empty.FollowsGlobalCheckin() {
		t.Error("无签到任务的渠道不应跟随全局签到时刻")
	}
}

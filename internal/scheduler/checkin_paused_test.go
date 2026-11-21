package scheduler

// 账号级签到暂停：所有签到路径都必须经 checkinOne 的门禁，
// 暂停期间不向供应商发请求，也不污染签到状态/不占重试窗口。

import (
	"testing"

	"wild-work/internal/provider"
)

// TestPausedAccountSkipsAllCheckinPaths 定时批次、自动重试点、手动单账号、批量入口
// 四条路径都不该给已暂停的账号发起签到请求。
func TestPausedAccountSkipsAllCheckinPaths(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, p := newTwoAccountScheduler(up)
	p.SetCheckinDisabled("uA", true)

	s.runCheckin(10 * 60) // 定时批次
	if got := up.count("uA"); got != 0 {
		t.Fatalf("定时批次仍请求了暂停账号 uA：%d 次", got)
	}
	if got := up.count("uB"); got != 1 {
		t.Fatalf("未暂停账号 uB 应照常签到，实际 %d 次", got)
	}
	if s.checkinSlotDone(10 * 60) {
		t.Fatal("uB 仍 no_campaign，时段不该标记完成")
	}

	s.runCheckin(10*60 + 1) // 窗口内重试
	if got := up.count("uA"); got != 0 {
		t.Fatalf("重试仍请求了暂停账号 uA：%d 次", got)
	}

	r, err := s.CheckinAccount("uA") // 手动单账号
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if !r.Paused || r.OK || r.Retryable || r.Msg != "已暂停签到" {
		t.Fatalf("手动签到暂停结果=%+v", r)
	}
	if got := up.count("uA"); got != 0 {
		t.Fatalf("手动签到仍请求了暂停账号 uA：%d 次", got)
	}

	s.RunCheckinNow() // 面板「签到全部」入口（start=-1）
	if got := up.count("uA"); got != 0 {
		t.Fatalf("签到全部仍请求了暂停账号 uA：%d 次", got)
	}
}

// TestPausedAccountNotMarkedDone 暂停期间不写「本时段已完成」，恢复后首轮就能签到。
func TestPausedAccountNotMarkedDone(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, p := newTwoAccountScheduler(up)
	p.SetCheckinDisabled("uA", true)

	s.runCheckin(10 * 60)
	if s.accountCheckinSlotDone(10*60, "uA") {
		t.Fatal("暂停账号不该被标记为已完成")
	}

	p.SetCheckinDisabled("uA", false)
	s.runCheckin(10 * 60)
	if got := up.count("uA"); got != 1 {
		t.Fatalf("恢复后应能立即签到，实际 %d 次", got)
	}
}

// TestPausedAccountDoesNotRecordCheckin 暂停不得污染面板的 last_checkin 状态。
func TestPausedAccountDoesNotRecordCheckin(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, p := newTwoAccountScheduler(up)
	p.SetCheckinDisabled("uA", true)

	s.runCheckin(10 * 60)
	st, _ := p.Status("uA")
	if st.LastCheckinOK || !st.LastCheckinAt.IsZero() {
		t.Fatalf("暂停账号的签到状态被写入：%+v", st)
	}
}

// TestPausedFlagExposed Paused 标记要透出给上层，供面板显示「已暂停」而非失败。
func TestPausedFlagExposed(t *testing.T) {
	up := newPerAccountUpstream(func(uid string) provider.CheckinStatus {
		return provider.CheckinClaimed
	})
	s, p := newTwoAccountScheduler(up)
	p.SetCheckinDisabled("uA", true)
	r, err := s.CheckinAccount("uA")
	if err != nil || !r.Paused || r.Msg != "已暂停签到" {
		t.Fatalf("Paused 标记未透出：%+v err=%v", r, err)
	}
}

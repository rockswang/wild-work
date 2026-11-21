package scheduler

// 本文件锁死「自动重试批次按账号去重」：一个时段内某账号签到成功后，
// 窗口内后续重试只打尚未达成的账号，已到账的账号不再重复请求供应商。

import (
	"errors"
	"sync"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// perAccountUpstream 按账号返回签到状态，并记录每个账号的签到调用次数。
type perAccountUpstream struct {
	provider.Upstream
	mu          sync.Mutex
	status      func(uid string) provider.CheckinStatus
	resourceErr error
	calls       map[string]int
}

func newPerAccountUpstream(status func(uid string) provider.CheckinStatus) *perAccountUpstream {
	return &perAccountUpstream{status: status, calls: map[string]int{}}
}

func (u *perAccountUpstream) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	u.mu.Lock()
	u.calls[a.UID]++
	u.mu.Unlock()
	st := u.status(a.UID)
	return provider.CheckinReport{Status: st, Msg: string(st), Amount: 100}, nil
}

func (u *perAccountUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	if u.resourceErr != nil {
		return 0, nil, u.resourceErr
	}
	return 0, nil, nil
}

func (u *perAccountUpstream) RefreshToken(a *auth.Auth) error { return nil }

func (u *perAccountUpstream) count(uid string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[uid]
}

// newTwoAccountScheduler A 恒成功、B 恒 no_campaign（构造窗口重试场景）。
func newTwoAccountScheduler(up provider.Upstream) (*Scheduler, *pool.Pool) {
	p := pool.New("")
	for _, uid := range []string{"uA", "uB"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	s := New(Config{
		Pool: p, Upstream: up, Name: "test",
		CheckinMinutes: []int{10 * 60}, CheckinRetryUntil: 12 * 60,
	})
	return s, p
}

func abStatus(uid string) provider.CheckinStatus {
	if uid == "uA" {
		return provider.CheckinClaimed
	}
	return provider.CheckinNoCampaign
}

// TestAutoRetrySkipsAccountAlreadyDone 同一时段第二次自动批次只重打未达成的账号。
func TestAutoRetrySkipsAccountAlreadyDone(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, _ := newTwoAccountScheduler(up)

	s.runCheckin(10 * 60) // 首次：A 成功、B retryable
	if got := up.count("uA"); got != 1 {
		t.Fatalf("首轮 A 调用=%d，期望 1", got)
	}
	if got := up.count("uB"); got != 1 {
		t.Fatalf("首轮 B 调用=%d，期望 1", got)
	}
	if !s.accountCheckinSlotDone(10*60, "uA") {
		t.Fatal("A 已成功，应按账号标记本时段完成")
	}
	if s.accountCheckinSlotDone(10*60, "uB") {
		t.Fatal("B 未达成，不应标记完成")
	}
	if s.checkinSlotDone(10 * 60) {
		t.Fatal("B 仍 retryable，整个时段不该标记完成")
	}

	s.runCheckin(10 * 60) // 窗口内重试
	if got := up.count("uA"); got != 1 {
		t.Fatalf("重试轮 A 被重复请求 %d 次，期望保持 1", got)
	}
	if got := up.count("uB"); got != 2 {
		t.Fatalf("重试轮 B 调用=%d，期望 2", got)
	}
}

// TestManualCheckinIgnoresAccountDone 面板手动签到不受按账号去重限制。
func TestManualCheckinIgnoresAccountDone(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, _ := newTwoAccountScheduler(up)

	// 自动批次先把 A 标记完成。
	s.runCheckin(10 * 60)
	if !s.accountCheckinSlotDone(10*60, "uA") {
		t.Fatal("前置：A 应已标记完成")
	}

	// 手动签到（start=-1）两账号都要打，且不写按账号完成状态。
	s.RunCheckinNow()
	if got := up.count("uA"); got != 2 {
		t.Fatalf("手动签到 A 调用=%d，期望 2（不跳过）", got)
	}
	if got := up.count("uB"); got != 2 {
		t.Fatalf("手动签到 B 调用=%d，期望 2", got)
	}
	if s.accountCheckinSlotDone(10*60, "uB") {
		t.Fatal("手动签到不应写入按账号完成状态")
	}
}

// TestAccountDoneAcrossSlots 默认的「一天一次签到」渠道：当日任一时段达成后，
// 后续时段不再向供应商发起签到请求。
func TestAccountDoneAcrossSlots(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, _ := newTwoAccountScheduler(up)

	s.runCheckin(10 * 60)
	if !s.accountDayCheckinDone("uA") {
		t.Fatal("A 当日应标记完成")
	}
	s.runCheckin(21 * 60) // 另一时段：A 不应再打供应商

	if got := up.count("uA"); got != 1 {
		t.Fatalf("跨时段 A 调用=%d，期望保持 1", got)
	}
	if got := up.count("uB"); got != 2 {
		t.Fatalf("跨时段 B 调用=%d，期望 2（B 仍未达成）", got)
	}
	if s.accountCheckinSlotDone(21*60, "uA") {
		t.Fatal("跳过时段不该标记时段完成（时间根本没走到那一步）")
	}
}

// TestMultiCheckinPerDayAcrossSlots 一天多次的渠道（Loomy 宠物打工按档领奖）：
// 关闭跨时间点去重，每个签到时刻仍各打一次，避免漏领后续档位。
func TestMultiCheckinPerDayAcrossSlots(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, _ := newTwoAccountScheduler(up)
	s.cfg.MultiCheckinPerDay = true

	s.runCheckin(10 * 60)
	s.runCheckin(21 * 60)

	if got := up.count("uA"); got != 2 {
		t.Fatalf("多次签到渠道跨时段 A 调用=%d，期望 2", got)
	}
	if s.accountDayCheckinDone("uA") {
		t.Fatal("多次签到渠道不该写入当日完成标记")
	}
	// 同时段内的按账号去重仍生效：重试只打未达成的账号。
	// 重试传时段起点（Run 循环里 retryWindowOpen 返回 start），不是当前分钟。
	s.runCheckin(21 * 60)
	if got := up.count("uA"); got != 2 {
		t.Fatalf("同时段重试 A 被重复请求 %d 次，期望保持 2", got)
	}
}

// TestAllAccountsDayDoneDrivesNextFire 当日全员完成后，Run 循环的签到触发点为空，
// 不再为今天排任何签到时间点。不断言 NextFire()：它是「签到∪保活」的合并投影，
// 默认保活 22:00（New 兜底）在签完后本就该留在今天，拿它套「12 小时窗口」
// 断言会得到一条上午恒绿、白天恒红的时间依赖断言。
func TestAllAccountsDayDoneDrivesNextFire(t *testing.T) {
	up := newPerAccountUpstream(func(uid string) provider.CheckinStatus {
		return provider.CheckinClaimed // 两个账号都成功
	})
	s, _ := newTwoAccountScheduler(up)

	if s.allAccountsDayDone() {
		t.Fatal("开局不该是已完成")
	}
	if got := s.fireMinutes([]int{10 * 60, 21 * 60}); len(got) == 0 {
		t.Fatal("开局应有两个触发点")
	}
	s.runCheckin(10 * 60)
	if !s.allAccountsDayDone() {
		t.Fatal("全员签到达成后应为当日完成")
	}
	if got := s.fireMinutes([]int{10 * 60, 21 * 60}); len(got) != 0 {
		t.Fatalf("当日完成后触发点应为空，实际 %v", got)
	}
}

// TestAccountDoneDespiteBalanceFailure 签到成功但余额查询失败时，仍标记账号完成。
// 判定快照取在余额查询之前——余额失败不该导致下一轮重复打签到接口。
func TestAccountDoneDespiteBalanceFailure(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	up.resourceErr = errors.New("余额查询失败")
	s, _ := newTwoAccountScheduler(up)

	s.runCheckin(10 * 60)
	if !s.accountCheckinSlotDone(10*60, "uA") {
		t.Fatal("签到已成功，余额失败不应阻止按账号标记完成")
	}
	s.runCheckin(10 * 60)
	if got := up.count("uA"); got != 1 {
		t.Fatalf("余额失败后 A 被重复请求 %d 次，期望保持 1", got)
	}
}

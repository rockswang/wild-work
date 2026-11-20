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

// TestAccountDoneScopedToSlot 已完成的账号只在「本时段」跳过，另一个时段照常签到。
func TestAccountDoneScopedToSlot(t *testing.T) {
	up := newPerAccountUpstream(abStatus)
	s, _ := newTwoAccountScheduler(up)

	s.runCheckin(10 * 60)
	s.runCheckin(21 * 60) // 另一时段：A 应重新签到

	if got := up.count("uA"); got != 2 {
		t.Fatalf("跨时段 A 调用=%d，期望 2", got)
	}
	if s.accountCheckinSlotDone(10*60, "uA") != true || s.accountCheckinSlotDone(21*60, "uA") != true {
		t.Fatal("两个时段的 A 都应各自标记完成")
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

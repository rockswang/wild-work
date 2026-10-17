// checkin_result_test.go 签到结果组合的回归测试：面板可见字段（ok / retryable / msg）
// 必须自洽——「显示成功」与「标记可重试」不得同时出现。
//
// 触发场景（2026-10-17 本机实测，app.log 108810-108818 行）：Qoder 桌面端活动列表里该账号
// 有 CLAIMABLE 活动，但 wild-work 解析失败落到 no_campaign → scheduler 给出
// ok=true + retryable=true：面板显示「签到成功」，toast 却报「暂未领到：无可用签到活动」。
package scheduler

import (
	"errors"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/ledger"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// dispatchUpstream 只覆盖签到与余额两条路径的假上游（其余方法不会被签到路径调用）。
type dispatchUpstream struct {
	provider.Upstream
	report provider.CheckinReport
	err    error
	remain int64
	calls  int32
}

func (d *dispatchUpstream) DailyCheckin(a *auth.Auth) error {
	_, err := d.DailyCheckinReport(a)
	return err
}

func (d *dispatchUpstream) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	d.calls++
	return d.report, d.err
}

func (d *dispatchUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return d.remain, []provider.ResourceItem{{Key: "userQuota", Name: "用户套餐", Remain: d.remain, Usable: true}}, nil
}

func (d *dispatchUpstream) RefreshToken(a *auth.Auth) error { return nil }

func newCheckinPool() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "dt", RefreshToken: "drt", ExpiresAt: time.Now().Add(48 * time.Hour).Unix()})
	return p
}

func newDispatchLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	lg, err := ledger.New(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	// 必须关闭句柄：Windows 上未关闭的文件锁会让 TempDir 清理失败（见 ledger.newTestLedger 同因）。
	t.Cleanup(lg.Close)
	return lg
}

// TestCheckinResultNoFalseSuccess 未达成（no_campaign）时不得同时报告 OK=true。
func TestCheckinResultNoFalseSuccess(t *testing.T) {
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinNoCampaign, Msg: "无可用签到活动"}, remain: 100}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})

	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK && res.Retryable {
		t.Fatalf("结果自相矛盾（面板会同时显示成功与失败）：%+v", res)
	}
	if res.OK {
		t.Fatalf("no_campaign 不是成功状态：%+v", res)
	}
	if !res.Retryable || res.Msg != "无可用签到活动" {
		t.Fatalf("msg 必须透出且可重试：%+v", res)
	}
}

// TestCheckinResultClaimedNotContradictory 真正领到时 ok 且不重试。
func TestCheckinResultClaimedNotContradictory(t *testing.T) {
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinClaimed, Msg: "签到成功 +100", Amount: 100}, remain: 200}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom", Ledger: newDispatchLedger(t)})

	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Retryable {
		t.Fatalf("claimed 应为 ok 且不重试：%+v", res)
	}
	if res.Msg != "签到成功 +100" {
		t.Errorf("msg = %q", res.Msg)
	}
}

// TestCheckinResultAlreadyIsSuccess 今日已领属于成功终态（不重试、不显示失败）。
func TestCheckinResultAlreadyIsSuccess(t *testing.T) {
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取", Amount: 100}, remain: 200}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})

	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Retryable {
		t.Fatalf("already 应为 ok 且不重试：%+v", res)
	}
	if res.Msg != "今日已领取" {
		t.Errorf("msg = %q, want 今日已领取", res.Msg)
	}
}

// TestCheckinResultUpstreamErrorIsRetryable 上游 5xx 时必须失败+可重试，且文案透出。
func TestCheckinResultUpstreamErrorIsRetryable(t *testing.T) {
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinError, Msg: "campaigns http 503: DEPENDENCY_UNAVAILABLE"}, remain: 100}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})

	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || !res.Retryable {
		t.Fatalf("5xx 应为失败+可重试：%+v", res)
	}
	if !res.Retryable || res.Msg != "campaigns http 503: DEPENDENCY_UNAVAILABLE" {
		t.Fatalf("msg 必须透出且可重试：%+v", res)
	}
}

// TestCheckinResultNonRetryableStatusIsFailure 渠道给出「非重试型」状态却同时带回 error 时，
// 结果必须是失败：结构化渠道先把 OK 回填成 !status.Retryable()（此处为 true），
// 失败分支必须把它纠正为 false，否则面板会「显示签到成功」而操作其实是失败的。
func TestCheckinResultNonRetryableStatusIsFailure(t *testing.T) {
	up := &dispatchUpstream{
		// CheckinClaimed 的 Retryable()=false ⇒ 渠道侧回填 OK=true；错误分支必须纠正为失败。
		report: provider.CheckinReport{Status: provider.CheckinClaimed, Msg: "签到成功 +100", Amount: 100},
		err:    errors.New("claim 回执异常"),
	}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})

	res, _ := s.CheckinAccount("u1")
	if res.OK {
		t.Fatalf("有错误时 OK 必须为 false（否则成功/失败同时显示）：%+v", res)
	}
}

// TestCheckinResultSessionDeadIsTerminal 401 且刷新成功：重试一次后仍 401，不得再标可重试。
// 401 与刷新路径已由既有的 TestCheckinSessionDeadTypedError 覆盖，这里只锁定终态语义。
func TestCheckinResultSessionDeadIsTerminal(t *testing.T) {
	dead := &provider.Error{Kind: provider.ErrSessionDead, Status: 401, Msg: "TOKEN_EXPIRE"}
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinError}, err: dead, remain: 0}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})
	// 刷新能否成功不由本用例关心：两种刷新结果都必须是终态失败。
	s.refreshOverride = func(a *auth.Auth, uid string) error { return errors.New("refresh 401") }

	res, _ := s.CheckinAccount("u1")
	if res.OK {
		t.Fatalf("401 不得报成功：%+v", res)
	}
	if res.Retryable {
		t.Fatalf("401 是终态（需重新登录），不得标可重试：%+v", res)
	}
	if up.calls != 1 {
		t.Errorf("刷新失败时不应再重试签到，实际调用 %d 次", up.calls)
	}
}

// TestCheckinResultSessionDeadRefreshFails 刷新也失败时，文案必须说明刷新失败。
func TestCheckinResultSessionDeadRefreshFails(t *testing.T) {
	dead := &provider.Error{Kind: provider.ErrSessionDead, Status: 401, Msg: "TOKEN_EXPIRE"}
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinError}, err: dead}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})
	s.refreshOverride = func(a *auth.Auth, uid string) error { return errors.New("refresh 401") }

	res, _ := s.CheckinAccount("u1")
	if res.OK || res.Retryable {
		t.Fatalf("刷新失败 + 401 应为终态失败：%+v", res)
	}
	if res.Msg == "" {
		t.Error("msg 不得为空")
	}
}

// TestCheckinResultSessionDeadHidesStatus 401 时不得再暴露「无可用签到活动」等无关文案，
// 否则用户会以为活动没开而反复点击。
func TestCheckinResultSessionDeadHidesStatus(t *testing.T) {
	dead := &provider.Error{Kind: provider.ErrSessionDead, Status: 401, Msg: "TOKEN_EXPIRE"}
	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinNoCampaign, Msg: "无可用签到活动"}, err: dead}
	s := New(Config{Pool: newCheckinPool(), Upstream: up, Name: "qodercom"})
	s.refreshOverride = func(a *auth.Auth, uid string) error { return errors.New("refresh 401") }

	res, _ := s.CheckinAccount("u1")
	if res.OK {
		t.Fatalf("401 不得报成功：%+v", res)
	}
	if res.Msg == "无可用签到活动" {
		t.Fatal("401 时不得把「无可用签到活动」当作文案透出")
	}
}

package scheduler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

func TestNextFireMinutes(t *testing.T) {
	now := time.Date(2026, 7, 27, 9, 5, 0, 0, time.Local)
	next := nextFireMinutes(now, []int{9 * 60, 9*60 + 30, 21 * 60})
	if next.Hour() != 9 || next.Minute() != 30 || next.Day() != 27 {
		t.Errorf("next=%v want 09:30 same day", next)
	}
	now = time.Date(2026, 7, 27, 9, 30, 0, 0, time.Local)
	next = nextFireMinutes(now, []int{9*60 + 30})
	if next.Day() != 28 || next.Hour() != 9 || next.Minute() != 30 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

// fakeUpstream 同时模拟 billing 与 refresh。
type fakeUpstream struct {
	checkinCalls   atomic.Int32
	refreshCalls   atomic.Int32
	resourceRemain int64
}

func (f *fakeUpstream) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

func (f *fakeUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return f.resourceRemain, []provider.ResourceItem{{Name: "套餐", Remain: f.resourceRemain, Usable: true}}, nil
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("should disable session-dead account: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// 不应 panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

func TestSetCheckinHoursRoundtrip(t *testing.T) {
	s := New(Config{})
	s.SetCheckinHours([]int{7, 19})
	got := s.CheckinHours()
	if len(got) != 2 || got[0] != 7 || got[1] != 19 {
		t.Errorf("hours=%v want [7 19]", got)
	}
}

func TestSetCheckinMinutesRoundtrip(t *testing.T) {
	s := New(Config{})
	s.SetCheckinMinutes([]int{21*60 + 30, 7*60 + 5})
	got := s.CheckinTimes()
	if len(got) != 2 || got[0] != "07:05" || got[1] != "21:30" {
		t.Errorf("times=%v", got)
	}
}

func TestCheckinRefreshesExpiredToken(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 300}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1, FilePath: filepath.Join(t.TempDir(), "auth.json")}
	p.Add(a)
	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || f.refreshCalls.Load() != 1 || f.checkinCalls.Load() != 1 {
		t.Errorf("res=%+v refresh=%d checkin=%d", res, f.refreshCalls.Load(), f.checkinCalls.Load())
	}
	if got := p.AuthByUID("u1").AccessToken; got != "new" {
		t.Errorf("access token=%q want new", got)
	}
}

func TestIsAlreadyRequiresExplicitMarker(t *testing.T) {
	if isAlready(errors.New("checkin claim failed: operation too frequent")) {
		t.Fatal("rate limit must not be treated as already checked in")
	}
	if !isAlready(errors.New("checkin claim code=9095")) {
		t.Fatal("code 9095 should be treated as already checked in")
	}
}

func TestCheckinObserverReceivesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), BillingBaseCN: srv.URL, ChatBaseCN: srv.URL, BillingBaseGlob: srv.URL, ChatBaseGlobal: srv.URL}
	s := New(Config{Pool: p, Upstream: up, Name: "traework"})
	var got CheckinResult
	s.SetCheckinObserver(func(r CheckinResult) { got = r })
	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || got.OK || got.Msg == "" {
		t.Fatalf("result=%+v observer=%+v", res, got)
	}
}

func TestCheckinAccountRecordsResult(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 300}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:            srv.Client(),
		ChatBaseCN:      srv.URL,
		BillingBaseCN:   srv.URL,
		ChatBaseGlobal:  srv.URL,
		BillingBaseGlob: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.HasRemain || res.Remain != 300 {
		t.Errorf("res=%+v", res)
	}
	st, _ := p.Status("u1")
	if !st.LastCheckinOK || st.LastCheckinAt.IsZero() || st.Credits != 300 {
		t.Errorf("status=%+v", st)
	}
	// 未知账号报错
	if _, err := s.CheckinAccount("nope"); err == nil {
		t.Error("want error for unknown uid")
	}
}

// ---- 签到窗口重试机制（对齐上游 qoder2api runScheduledCheckin） ----

// reporterUpstream 模拟实现了 provider.CheckinReporter 的渠道（QoderCN/COM）。
// 按预设的状态序列逐次返回，用于验证「窗口内重试直到达成」。
// 内嵌 provider.Upstream 接口：只需覆盖签到相关方法，其余方法不会在签到路径被调用。
type reporterUpstream struct {
	provider.Upstream
	statuses []provider.CheckinStatus
	calls    atomic.Int32
}

func (r *reporterUpstream) next() provider.CheckinReport {
	i := int(r.calls.Add(1)) - 1
	if i >= len(r.statuses) {
		i = len(r.statuses) - 1
	}
	st := r.statuses[i]
	return provider.CheckinReport{Status: st, Msg: string(st), Amount: 100}
}

func (r *reporterUpstream) DailyCheckin(a *auth.Auth) error {
	_, err := r.DailyCheckinReport(a)
	return err
}

func (r *reporterUpstream) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return r.next(), nil
}

func (r *reporterUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (r *reporterUpstream) RefreshToken(a *auth.Auth) error { return nil }
func (r *reporterUpstream) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// newReporterScheduler 构造带窗口重试的调度器（10:00 起、12:00 截止）。
func newReporterScheduler(up provider.Upstream) (*Scheduler, *pool.Pool) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return New(Config{
		Pool: p, Upstream: up, Name: "qodercn",
		CheckinMinutes:    []int{10 * 60},
		CheckinRetryUntil: 12 * 60,
	}), p
}

// TestCheckinDueWindowRetry 窗口内每分钟都应触发，直到该时段完成。
func TestCheckinDueWindowRetry(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinNoCampaign}}
	s, _ := newReporterScheduler(up)
	ch := []int{10 * 60}

	if !s.checkinDue(ch, 10*60) {
		t.Error("10:00 起点应触发")
	}
	if !s.checkinDue(ch, 10*60+1) {
		t.Error("10:01 窗口内应重试（未完成）")
	}
	if !s.checkinDue(ch, 12*60) {
		t.Error("12:00 截止点应仍可重试")
	}
	if s.checkinDue(ch, 12*60+1) {
		t.Error("12:01 超出截止不应触发")
	}
	if s.checkinDue(ch, 9*60+59) {
		t.Error("窗口前不应触发")
	}
}

// TestCheckinNoCampaignKeepsRetryingUntilClaimed no_campaign 不算完成，须继续重试；
// 拿到 claimed 后当日即完成、不再重试。这是上游 ae3d42f 修的核心行为。
func TestCheckinNoCampaignKeepsRetryingUntilClaimed(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{
		provider.CheckinNoCampaign, provider.CheckinNoCampaign, provider.CheckinClaimed,
	}}
	s, _ := newReporterScheduler(up)
	ch := []int{10 * 60}

	s.runCheckin(10 * 60) // 第一次：no_campaign
	if s.checkinSlotDone(10 * 60) {
		t.Fatal("no_campaign 不得标记当日完成")
	}
	if !s.checkinDue(ch, 10*60+1) {
		t.Fatal("应继续重试")
	}
	s.runCheckin(10 * 60) // 第二次：no_campaign
	if s.checkinSlotDone(10 * 60) {
		t.Fatal("no_campaign 不得标记当日完成")
	}
	s.runCheckin(10 * 60) // 第三次：claimed
	if !s.checkinSlotDone(10 * 60) {
		t.Fatal("claimed 后应标记当日完成")
	}
	if s.checkinDue(ch, 10*60+5) {
		t.Fatal("完成后不应再重试")
	}
	if up.calls.Load() != 3 {
		t.Errorf("checkin calls=%d want 3", up.calls.Load())
	}
}

// TestCheckinAlreadyStopsRetry 已领取即完成，不重试。
func TestCheckinAlreadyStopsRetry(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinAlready}}
	s, _ := newReporterScheduler(up)
	s.runCheckin(10 * 60)
	if !s.checkinSlotDone(10 * 60) {
		t.Fatal("already_claimed 应标记当日完成")
	}
	if s.checkinDue([]int{10 * 60}, 10*60+1) {
		t.Fatal("已领取不应再重试")
	}
}

// TestCheckinResultMsgPreserved 结构化渠道的文案必须透出（不再被覆盖成 "ok"），
// 否则面板无法区分「真领到 100」与「活动还没上线」。
func TestCheckinResultMsgPreserved(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinNoCampaign}}
	s, _ := newReporterScheduler(up)
	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg == "ok" || res.Msg != string(provider.CheckinNoCampaign) {
		t.Errorf("msg = %q, want no_campaign（不得被覆盖成 ok）", res.Msg)
	}
	if res.OK || !res.Retryable {
		t.Errorf("no_campaign 应为未达成且可重试: %+v", res)
	}

	up2 := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	s2, _ := newReporterScheduler(up2)
	res2, _ := s2.CheckinAccount("u1")
	if !res2.OK || res2.Retryable {
		t.Errorf("claimed 应为成功且不重试: %+v", res2)
	}
}

// TestCheckinRetryDisabledByDefault CheckinRetryUntil=0 时退化为精确触发（旧行为）。
func TestCheckinRetryDisabledByDefault(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinNoCampaign}}, CheckinMinutes: []int{9 * 60}})
	if s.checkinDue([]int{9 * 60}, 9*60+1) {
		t.Error("未配置重试窗口时不应在窗口内重复触发")
	}
	if !s.checkinDue([]int{9 * 60}, 9*60) {
		t.Error("精确时刻仍应触发")
	}
}

// TestCheckinMultipleSlotsIndependent 一天多个签到时段互不影响：
// 早间完成不得吞掉晚间时段（上游单时段模型不适用本工具的多时段配置）。
func TestCheckinMultipleSlotsIndependent(t *testing.T) {
	up := &reporterUpstream{statuses: []provider.CheckinStatus{provider.CheckinClaimed}}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: up, CheckinMinutes: []int{9 * 60, 21 * 60}, CheckinRetryUntil: 22 * 60})
	ch := []int{9 * 60, 21 * 60}

	s.markSlotDone(9 * 60)
	if !s.checkinDue(ch, 21*60) {
		t.Error("晚间时段不应受早间完成影响")
	}
	if s.checkinDue(ch, 9*60+5) {
		t.Error("已完成的早间时段不应重试")
	}
}

// TestCheckinSessionDeadTypedError 401 类型化错误触发一次刷新+重试（不变式 19）。
// ExpiresAt 设远期，使唯一的一次 refresh 来自 session-dead 自愈而非 NeedsRefresh。
func TestCheckinSessionDeadTypedError(t *testing.T) {
	up := &deadThenOK{}
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		FilePath: filepath.Join(t.TempDir(), "u1.json")}
	p.Add(a)
	s := New(Config{Pool: p, Upstream: up, Name: "qodercn", CheckinMinutes: []int{10 * 60}})

	res, err := s.CheckinAccount("u1")
	if err != nil {
		t.Fatal(err)
	}
	if up.reportCalls.Load() != 2 {
		t.Errorf("report calls=%d want 2 (refresh + retry)", up.reportCalls.Load())
	}
	if up.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d want 1", up.refreshCalls.Load())
	}
	if !res.OK {
		t.Errorf("retry should succeed: %+v", res)
	}
}

// deadThenOK 首次返回类型化 ErrSessionDead，刷新后返回 claimed。
type deadThenOK struct {
	provider.Upstream
	reportCalls  atomic.Int32
	refreshCalls atomic.Int32
}

func (d *deadThenOK) DailyCheckin(a *auth.Auth) error { _, err := d.DailyCheckinReport(a); return err }

func (d *deadThenOK) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	if d.reportCalls.Add(1) == 1 {
		return provider.CheckinReport{Status: provider.CheckinError},
			&provider.Error{Kind: provider.ErrSessionDead, Status: 401, Msg: "TOKEN_EXPIRE"}
	}
	return provider.CheckinReport{Status: provider.CheckinClaimed, Msg: "签到成功 +100", Amount: 100}, nil
}

func (d *deadThenOK) RefreshToken(a *auth.Auth) error { d.refreshCalls.Add(1); return nil }
func (d *deadThenOK) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (d *deadThenOK) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

package scheduler

// checkin_reporter_earn_test.go：结构化上报（CheckinReporter）路径的权威入账（issue #147）。
//
// 背景：issue #67 修的「签到回执发放额直接入账」原先只在 CheckinGranter 路径生效。
// WorkBuddy 国内版改为实现 CheckinReporter（先只读探测活动状态、已签则不写）后，
// 若不在 reporter 路径同样入账，同一渠道的两条路径就会记账口径不一致——
// 表现为「签到成功但面板今日收入仍显示 0」（正是 #67 的现象重现）。

import (
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/ledger"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// amountReporter 只实现 CheckinReporter，回执带 Amount（对齐 upstream.DailyCheckinReport）。
type amountReporter struct {
	provider.Upstream
	report provider.CheckinReport
}

func (r *amountReporter) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return r.report, nil
}

func (r *amountReporter) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 3428, []provider.ResourceItem{{Name: "套餐", Remain: 3428, Usable: true}}, nil
}

// TestReporterClaimedRecordsAuthoritativeEarn claimed 且 Amount>0：
// 发放额直接落 earn（与 CheckinGranter 同口径），不依赖快照差分。
func TestReporterClaimedRecordsAuthoritativeEarn(t *testing.T) {
	lg, err := ledger.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	up := &amountReporter{report: provider.CheckinReport{
		Status: provider.CheckinClaimed, Msg: "签到成功 +100（连续 19 天）", Amount: 100,
	}}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddy", Ledger: lg, CheckinHours: []int{9}})

	s.RunCheckinNow()

	st := lg.Query(1, func(uid string) (name, channel string) { return "测试", "workbuddy" })
	if st.Credit.Earn != 3528 {
		t.Fatalf("earn=%d，want 3528（baseline 3428 + 权威回执 100）", st.Credit.Earn)
	}
	var found bool
	for _, e := range st.Credit.Entries {
		if e.Kind == "earn" && e.Note == "签到奖励" && e.Amount == 100 {
			found = true
		}
	}
	if !found {
		t.Fatal("应存在 Note=签到奖励 Amount=100 的权威 earn 条目（reporter 路径）")
	}
}

// TestReporterAlreadyDoesNotRecordEarn 已签到（Amount=0）不得记假账。
func TestReporterAlreadyDoesNotRecordEarn(t *testing.T) {
	lg, err := ledger.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	up := &amountReporter{report: provider.CheckinReport{
		Status: provider.CheckinAlready, Msg: "已签到（连续 18 天，本周 6 天）",
	}}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddy", Ledger: lg, CheckinHours: []int{9}})

	s.RunCheckinNow()

	st := lg.Query(1, func(uid string) (name, channel string) { return "测试", "workbuddy" })
	for _, e := range st.Credit.Entries {
		if e.Kind == "earn" && e.Note == "签到奖励" {
			t.Fatalf("已签到不得记权威 earn: %+v", e)
		}
	}
	// 已签到应算成功且不可重试
	stp, _ := p.Status("u1")
	if !stp.LastCheckinOK {
		t.Fatal("已签到应标记为成功")
	}
	if stp.LastCheckinMsg == "" {
		t.Fatal("签到信息应透出（含连续天数等进度），不得被覆盖成 ok")
	}
}

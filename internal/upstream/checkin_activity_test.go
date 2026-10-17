// 签到活动探测（issue #147）的行为锁定。
//
// 上游实测回执（2026-10-17，Smurf7788，只读请求）作为固定样本：
//
//	POST /v2/billing/meter/checkin-activity-status {}
//	{"code":0,"data":{"active":true,"today_checked_in":true,"streak_days":18,
//	 "week_checkin_days":6,"daily_credit":100,"today_credit":100,
//	 "activity_name":"Buddy应用","start_time":"2026-09-30 00:00:00",
//	 "end_time":"2026-10-31 23:59:59", ...}}
//
// 本文件锁住的核心不变量：**已签到时不发写请求**（旧实现必然打一次 daily-checkin
// 换来 400+10001 的噪音），以及三种状态的语义映射。
package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// activityUpstream 假上游：按路径分发活动状态与领取结果，并统计写请求次数。
type activityUpstream struct {
	actBody    string // checkin-activity-status 回执
	actStatus  int
	claimBody  string // daily-checkin 回执
	claimCode  int
	claimCalls int // 写请求次数（核心断言）
	probeCalls int
}

func (u *activityUpstream) srv(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/billing/meter/checkin-activity-status", func(w http.ResponseWriter, r *http.Request) {
		u.probeCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.actStatus)
		_, _ = io.WriteString(w, u.actBody)
	})
	mux.HandleFunc("/v2/billing/meter/daily-checkin", func(w http.ResponseWriter, r *http.Request) {
		u.claimCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.claimCode)
		_, _ = io.WriteString(w, u.claimBody)
	})
	return httptest.NewServer(mux)
}

// newActivityClient 构造指向假上游的 Client（账单与对话基址同指）。
func newActivityClient(srvURL string) *Client {
	c := New()
	c.BillingBaseCN = srvURL
	c.ChatBaseCN = srvURL
	return c
}

// 已签到（生产实录样本）：必须报 Already，且**零写请求**。
func TestDailyCheckinReportAlreadyCheckedIn(t *testing.T) {
	u := &activityUpstream{
		actStatus: 200,
		actBody: `{"code":0,"data":{"active":true,"today_checked_in":true,"streak_days":18,` +
			`"week_checkin_days":6,"daily_credit":100,"today_credit":100,"total_credits":1800,` +
			`"activity_name":"Buddy应用","theme_name":"Buddy加油站",` +
			`"start_time":"2026-09-30 00:00:00","end_time":"2026-10-31 23:59:59"}}`,
	}
	srv := u.srv(t)
	defer srv.Close()

	rep, err := newActivityClient(srv.URL).DailyCheckinReport(&auth.Auth{UID: "u1", AccessToken: "t"})
	if err != nil {
		t.Fatalf("已签到不应返回 error（旧实现返回 400+10001 的 *Error），实际 %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v，want already_claimed", rep.Status)
	}
	if u.claimCalls != 0 {
		t.Fatalf("已签到时不得发写请求（daily-checkin 被调 %d 次）", u.claimCalls)
	}
	if u.probeCalls != 1 {
		t.Fatalf("应只探测一次活动状态，实际 %d 次", u.probeCalls)
	}
	// 面板可读的进度信息必须透出（旧路径只有一句 error 文案）
	if !strings.Contains(rep.Msg, "18") || !strings.Contains(rep.Msg, "6") {
		t.Errorf("Msg 应含连续天数与本周天数，实际 %q", rep.Msg)
	}
	if rep.Status.Retryable() {
		t.Error("already 不得可重试（否则窗口内会反复刷）")
	}
}

// 活动未开启：报 NoCampaign（可重试），且同样零写请求。
func TestDailyCheckinReportInactive(t *testing.T) {
	u := &activityUpstream{
		actStatus: 200,
		actBody:   `{"code":0,"data":{"active":false,"today_checked_in":false,"activity_name":"Buddy应用"}}`,
	}
	srv := u.srv(t)
	defer srv.Close()

	rep, err := newActivityClient(srv.URL).DailyCheckinReport(&auth.Auth{UID: "u1", AccessToken: "t"})
	if err != nil {
		t.Fatalf("活动未开启不应返回 error，实际 %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("Status = %v，want no_campaign", rep.Status)
	}
	if !rep.Status.Retryable() {
		t.Error("no_campaign 必须可重试（活动可能在整点后才创建）")
	}
	if u.claimCalls != 0 {
		t.Fatalf("活动未开启时不得发写请求，实际 %d 次", u.claimCalls)
	}
}

// 活动开启且未签到：此时才发写请求，回执金额进 Amount。
func TestDailyCheckinReportClaims(t *testing.T) {
	u := &activityUpstream{
		actStatus: 200,
		actBody: `{"code":0,"data":{"active":true,"today_checked_in":false,"streak_days":18,` +
			`"daily_credit":100,"week_checkin_days":6,"activity_name":"Buddy应用"}}`,
		claimCode: 200,
		claimBody: `{"code":0,"data":{"credit":100,"today_credit":100}}`,
	}
	srv := u.srv(t)
	defer srv.Close()

	rep, err := newActivityClient(srv.URL).DailyCheckinReport(&auth.Auth{UID: "u1", AccessToken: "t"})
	if err != nil {
		t.Fatalf("领取成功不应返回 error，实际 %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status = %v，want claimed", rep.Status)
	}
	if rep.Amount != 100 {
		t.Errorf("Amount = %d，want 100（回执 credit 是权威发放额）", rep.Amount)
	}
	if u.claimCalls != 1 {
		t.Fatalf("应发 1 次写请求，实际 %d 次", u.claimCalls)
	}
}

// 并发场景：探测说未签、写请求却回「今天已签到」→ 仍按已完成上报（不误判为失败）。
func TestDailyCheckinReportRaceAlready(t *testing.T) {
	u := &activityUpstream{
		actStatus: 200,
		actBody:   `{"code":0,"data":{"active":true,"today_checked_in":false,"daily_credit":100}}`,
		claimCode: 400,
		claimBody: `{"code":10001,"msg":"今天已签到，请明天再来","requestId":"x"}`,
	}
	srv := u.srv(t)
	defer srv.Close()

	rep, err := newActivityClient(srv.URL).DailyCheckinReport(&auth.Auth{UID: "u1", AccessToken: "t"})
	if err != nil {
		t.Fatalf("「今天已签到」不应作为 error 上抛，实际 %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v，want already_claimed（并发下别处刚签过）", rep.Status)
	}
}

// 探测端点不存在（上游撤掉/改路径）：必须降级为旧的直接领取，签到不能失效。
func TestDailyCheckinReportProbeFailsFallsBack(t *testing.T) {
	u := &activityUpstream{
		actStatus: 404,
		actBody:   `{"error":"not found"}`,
		claimCode: 200,
		claimBody: `{"code":0,"data":{"credit":100}}`,
	}
	srv := u.srv(t)
	defer srv.Close()

	rep, err := newActivityClient(srv.URL).DailyCheckinReport(&auth.Auth{UID: "u1", AccessToken: "t"})
	if err != nil {
		t.Fatalf("探测失败应降级而非报错，实际 %v", err)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Fatalf("降级路径应仍能领取，实际 Status=%v Amount=%d", rep.Status, rep.Amount)
	}
	if u.claimCalls != 1 {
		t.Fatalf("降级路径应发 1 次写请求，实际 %d 次", u.claimCalls)
	}
}

package modelscope

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// fakeClaimSite 假主站（票 04）：模拟「调 login/info 后当日才出现 EARN」的有状态行为。
//
// 为什么必须是有状态的：三态判定借道 transactions 复读（D4 选项 A），
// 「未领 vs 已领」的区别只体现在**领取前后**当日流水是否出现 EARN——
// 固定报文无法区分 claimed 与 already_claimed。loginStatus 非 0 时不置 claimed，
// 用于模拟上游拒绝。
type fakeClaimSite struct {
	mu          sync.Mutex
	claimed     bool  // 当日是否已发放（login/info 成功后置 true）
	noActivity  bool  // true = login/info 成功但**永不**发放（无活动）
	earn        int64 // 当日 EARN 金额
	loginStatus int   // 非 0 = login/info 返回该状态码
	loginHits   int
	txHits      int
	txCookies   []string
	loginCookie []string
}

func (f *fakeClaimSite) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(EpMagicubesTransactions, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.txHits++
		f.txCookies = append(f.txCookies, r.Header.Get("Authorization"))
		claimed := f.claimed
		earn := f.earn
		f.mu.Unlock()
		records := ""
		if claimed {
			records = fmt.Sprintf(`{"gmt_created":%q,"type":"EARN","rule_key":"daily_active","total_amount":%d,"count":1,"expire_at":"2099-01-02 10:00:00","expire_days":1}`,
				todayUTC8(), earn)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"success":true,"data":{"page":1,"page_size":20,"total":%d,"records":[%s]}}`,
			btoi(records != ""), records)))
	})
	mux.HandleFunc(EpLoginInfo, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.loginHits++
		f.loginCookie = append(f.loginCookie, r.Header.Get("Cookie"))
		status, noAct := f.loginStatus, f.noActivity
		if status == 0 && !noAct {
			f.claimed = true
		}
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			// 回显收到的凭据：验证 fetchSite 会把 token 从错误体里抹掉（零 token 落盘）。
			tok := strings.TrimPrefix(r.Header.Get("Cookie"), "m_session_id=")
			_, _ = fmt.Fprintf(w, `{"Code":%d,"Message":"token %s rejected","Success":false}`, status, tok)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Code":200,"Data":{"Name":"u"},"Message":"success","RequestId":"r","Success":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestDailyCheckinClaimed 「未领」→ claimed 且 Msg 含金额；领取动作是 login/info。
func TestDailyCheckinClaimed(t *testing.T) {
	f := &fakeClaimSite{earn: 200}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-tok")

	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "ms-tok"})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status = %q，期望 claimed", rep.Status)
	}
	if !strings.Contains(rep.Msg, "200") {
		t.Fatalf("Msg 必须透出金额：%q", rep.Msg)
	}
	if rep.Status.Retryable() {
		t.Fatal("claimed 不应可重试（当日已完成）")
	}
	// 金额只走 Msg：Amount 恒 0，避免与随后 DiffCredits 差分重复入账。
	if rep.Amount != 0 {
		t.Fatalf("Amount 必须为 0（金额只在 Msg；否则与差分记账重复入账）：%d", rep.Amount)
	}
	if f.loginHits != 1 {
		t.Fatalf("应恰好调一次 login/info，实际 %d", f.loginHits)
	}
	// 鉴权形态：流水走 Bearer，领取走 Cookie（D7）。
	for _, a := range f.txCookies {
		if a != "Bearer ms-tok" {
			t.Fatalf("transactions 应走 Bearer，实得 %q", a)
		}
	}
	if len(f.loginCookie) != 1 || f.loginCookie[0] != "m_session_id=ms-tok" {
		t.Fatalf("login/info 应走 Cookie: m_session_id，实得 %v", f.loginCookie)
	}
}

// TestDailyCheckinAlreadyClaimed 「已领」→ already_claimed，且**不再**调 login/info。
func TestDailyCheckinAlreadyClaimed(t *testing.T) {
	f := &fakeClaimSite{claimed: true, earn: 250}
	c := newSiteClient(provider.ModelScopeAI, f.start(t).URL, "ms-tok")

	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "ms-tok"})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %q，期望 already_claimed", rep.Status)
	}
	if !strings.Contains(rep.Msg, "250") {
		t.Fatalf("Msg 应带今日已领金额：%q", rep.Msg)
	}
	if f.loginHits != 0 {
		t.Fatalf("已领时不应再调 login/info（那是「已存在当日 EARN 不再调」的场景），实际 %d 次", f.loginHits)
	}
}

// TestDailyCheckinIdempotent 幂等：同一天连调两次，第二次为 already_claimed。
func TestDailyCheckinIdempotent(t *testing.T) {
	f := &fakeClaimSite{earn: 200}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-tok")
	a := &auth.Auth{AccessToken: "ms-tok"}

	first, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("第 1 次: %v", err)
	}
	second, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("第 2 次: %v", err)
	}
	if first.Status != provider.CheckinClaimed || second.Status != provider.CheckinAlready {
		t.Fatalf("两次状态应为 claimed→already_claimed，实得 %q→%q", first.Status, second.Status)
	}
	if f.loginHits != 1 {
		t.Fatalf("第二次不应重复领取，login/info 总次数应为 1，实际 %d", f.loginHits)
	}
}

// TestDailyCheckinNoCampaign 领取成功但上游没有可领的 ⇒ no_campaign（R150：不是失败）。
func TestDailyCheckinNoCampaign(t *testing.T) {
	f := &fakeClaimSite{noActivity: true}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-tok")

	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "ms-tok"})
	if err != nil {
		t.Fatalf("无活动不应是 error：%v", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("Status = %q，期望 no_campaign", rep.Status)
	}
	if !strings.Contains(rep.Msg, "上游还没有发放") {
		t.Fatalf("无活动文案应说明「上游还没有发放」：%q", rep.Msg)
	}
	if !rep.Status.Retryable() {
		t.Fatal("no_campaign 应在窗口内可重试")
	}
}

// TestDailyCheckinUpstreamError 领取失败 ⇒ error + 可读 Msg（不吞原文）。
func TestDailyCheckinUpstreamError(t *testing.T) {
	f := &fakeClaimSite{loginStatus: http.StatusInternalServerError}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-tok")

	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "ms-tok"})
	if err == nil {
		t.Fatal("5xx 应上抛错误")
	}
	if rep.Status != provider.CheckinError {
		t.Fatalf("Status = %q，期望 error", rep.Status)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("错误文案应带状态码（可行动）：%v", err)
	}
}

// TestDailyCheckinUnauthorizedIsSessionDead 401/403 ⇒ 类型化 ErrSessionDead（自愈入口）。
func TestDailyCheckinUnauthorizedIsSessionDead(t *testing.T) {
	f := &fakeClaimSite{loginStatus: http.StatusUnauthorized}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-tok")

	_, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "ms-tok"})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("应返回 *provider.Error，实得 %v", err)
	}
	if pe.Kind != provider.ErrSessionDead {
		t.Fatalf("Kind = %s，期望 ErrSessionDead", pe.Kind)
	}
}

// TestDailyCheckinDelegates DailyCheckin 与 DailyCheckinReport 必须走同一条链路。
func TestDailyCheckinDelegates(t *testing.T) {
	f := &fakeClaimSite{earn: 200}
	srv := f.start(t)
	c := newSiteClient(provider.ModelScope, srv.URL, "ms-tok")
	a := &auth.Auth{AccessToken: "ms-tok"}

	if err := c.DailyCheckin(a); err != nil {
		t.Fatalf("DailyCheckin: %v", err)
	}
	// 第一次已领，第二次走任一入口都应是 already（无 error）。
	if err := c.DailyCheckin(a); err != nil {
		t.Fatalf("DailyCheckin 第二次: %v", err)
	}
	// 同一状态机：DailyCheckin 也调了 login/info（仅首次）。
	if f.loginHits != 1 {
		t.Fatalf("DailyCheckin 应走同一链路（login/info 恰好 1 次），实际 %d", f.loginHits)
	}
}

// TestDailyCheckinNoTokenLeak 零 token 落盘（不变量 2）：上游把凭据回显在错误体里时，
// 进日志的错误必须已抹掉。
func TestDailyCheckinNoTokenLeak(t *testing.T) {
	const secret = "ms-0123456789abcdef0123456789abcdef"
	f := &fakeClaimSite{loginStatus: http.StatusForbidden}
	srv := f.start(t)
	c := newSiteClient(provider.ModelScope, srv.URL, secret)
	logs := captureLog(t)

	_, err := c.DailyCheckinReport(&auth.Auth{AccessToken: secret})
	if err == nil {
		t.Fatal("403 应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄露 token：%v", err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("日志泄露 token：%q", logs.String())
	}
}

// TestMagicubeTodayEarnTimezone 当日 EARN 只按 UTC+8 日期匹配，且只认 EARN。
func TestMagicubeTodayEarnTimezone(t *testing.T) {
	now := timeFromUTC8(t, "2026-11-16 09:00:00")
	tx := magicubesTransactionsData{Records: []magicubesTransaction{
		{GMTCreated: "2026-11-16", Type: "EARN", TotalAmt: 200},
		{GMTCreated: "2026-11-16", Type: "EARN", TotalAmt: 50},
		{GMTCreated: "2026-11-16", Type: "SPEND_CONFIRM", TotalAmt: 10},
		{GMTCreated: "2026-11-15", Type: "EARN", TotalAmt: 999},
		{GMTCreated: "2026-11-16", Type: "EXPIRE", TotalAmt: 88},
	}}
	amount, n := magicubeTodayEarn(tx, now)
	if amount != 250 || n != 2 {
		t.Fatalf("当日 EARN 合计/行数 = %d/%d，期望 250/2", amount, n)
	}
}

// todayUTC8 当前 UTC+8 日期串（假上游按它生成 gmt_created）。
func todayUTC8() string { return time.Now().In(creditLoc).Format("2006-01-02") }

// timeFromUTC8 按魔粒墙钟口径解析一个时刻（测试用）。
func timeFromUTC8(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation(magicubeExpireLayout, s, creditLoc)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", s, err)
	}
	return tm
}

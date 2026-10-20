package monkeycode

// checkin_test.go MonkeyCode 签到**三步流程**的契约测试（2026-10-17 按实测重写）。
//
// 与旧版的根本区别：旧版的每组断言锁的都是「作者自己猜的字段名与状态码」
// （reward / credits / already_checked_in / POST / 409），一旦真实上游不是那样，
// 测试全绿而线上必错。本版所有期望值都来自**实测报文**（见 checkin.go 文件头）：
//
//	GET  /api/v1/users/wallet/checkin            → {"code":0,…,"data":{"checked_in":false}}
//	POST /api/v1/public/captcha/challenge  {}    → {"challenge":{"c":…,"s":…,"d":…},"token":…}
//	POST /api/v1/public/captcha/redeem     {…}   → {"success":true,"token":"<id>:<ver>"}
//	POST /api/v1/users/wallet/checkin      {"captcha_token":"<ver>"} → {"code":0,…,"data":{"checked_in":true}}
//
// 假上游**自己独立校验**工作量证明（用 sha256 + hex 串比较，不复用 capHexPrefixOK），
// 因此 solutions 算法一错这里的用例就会红——这正是旧版缺的那道闸门。
//
// 全部离线：假上游 httptest，**不打真实上游**（AGENTS §0.1）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 假上游：按路径分派三步，并独立校验 PoW
// ---------------------------------------------------------------------------

const (
	fakeChallengeToken  = "0123456789abcdef01234567"
	fakeVerifyToken     = "deadbeef:00112233445566"
	fakeC, fakeS, fakeD = 4, 32, 3
)

type capCall struct {
	method string
	path   string
	cookie string
	body   string
}

type capUpstream struct {
	mu    sync.Mutex
	calls []capCall

	// 可配置行为
	checkedInBefore bool                        // 第①步 getCheckinState 的回值
	onState         func(w http.ResponseWriter) // 覆盖第①步回包（形状突变用例）
	onClaim         func(w http.ResponseWriter) // 覆盖第③步回包
	stateStatus     int                         // 非 0 时第①步直接回该 HTTP 状态
	claimStatus     int                         // 非 0 时第③步直接回该 HTTP 状态
	requireToken    string                      // 第③步必须收到的 captcha_token（默认 fakeVerifyToken）
	redeemFail      bool                        // redeem 是否返回失败（零值 = 成功，避开 Go 零值陷阱）
}

func (u *capUpstream) record(r *http.Request, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, capCall{r.Method, r.URL.Path, r.Header.Get("Cookie"), body})
}

func (u *capUpstream) snapshot() []capCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]capCall, len(u.calls))
	copy(out, u.calls)
	return out
}

// newCapUpstream 起假上游并把 Client.Console 指过去（签到走控制台域）。
func newCapUpstream(t *testing.T, u *capUpstream) *Client {
	t.Helper()
	if u.requireToken == "" {
		u.requireToken = fakeVerifyToken
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		u.record(r, string(raw))
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case epWalletCheckin:
			if r.Method == http.MethodGet {
				if u.stateStatus != 0 {
					w.WriteHeader(u.stateStatus)
				}
				if u.onState != nil {
					u.onState(w)
					return
				}
				fmt.Fprintf(w, `{"code":0,"message":"success","data":{"checked_in":%t}}`, u.checkedInBefore)
				return
			}
			// 第③步：领取。字段名写错就回真实上游那条 10701。
			if u.claimStatus != 0 {
				w.WriteHeader(u.claimStatus)
			}
			if u.onClaim != nil {
				u.onClaim(w)
				return
			}
			var body struct {
				CaptchaToken string `json:"captcha_token"`
			}
			_ = json.Unmarshal(raw, &body)
			if body.CaptchaToken != u.requireToken {
				fmt.Fprint(w, `{"code":10701,"message":"兑换验证码失败"}`)
				return
			}
			fmt.Fprint(w, `{"code":0,"message":"success","data":{"checked_in":true}}`)

		case epCaptchaChallenge:
			fmt.Fprintf(w, `{"challenge":{"c":%d,"s":%d,"d":%d},"expires":1792240465355,"token":%q}`,
				fakeC, fakeS, fakeD, fakeChallengeToken)

		case epCaptchaRedeem:
			if u.redeemFail {
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"success":false,"message":"invalid solutions"}`)
				return
			}
			var body struct {
				Token     string  `json:"token"`
				Solutions []int64 `json:"solutions"`
			}
			uerr := json.Unmarshal(raw, &body)
			if uerr != nil || !verifyCapSolutions(body.Token, body.Solutions) {
				// 与真实上游同款的失败应答（实测：HTTP 500 + invalid solutions）。
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"success":false,"message":"invalid solutions"}`)
				return
			}
			fmt.Fprintf(w, `{"success":true,"token":%q,"expires":1792240695758}`, fakeVerifyToken)

		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.Console = srv.URL
	return c
}

// fakeSalt / fakeTarget 是**假上游自己那套** salt/target，由 Node 侧 Cap 官方
// `prng.js` 预先算好并在此硬编码（token=`fakeChallengeToken`，c=4、s=32、d=3）。
//
// 为什么不在这里调 capPRNG 现算：那会让"独立校验"名不副实——实现的 PRNG 一旦写错，
// 假上游会**跟着一起错**，两边仍然自洽，整合用例照样全绿（2026-10-17 独立复核
// 用变异实测确认过这个盲区）。硬编码常量后，这两条整合用例真正独立于实现。
// ⚠️ 改动 fakeChallengeToken / fakeC / fakeS / fakeD 时必须同步重算本表。
var (
	fakeSalt = []string{
		"feae69ad40e1dc6016d1db169bd2b42f",
		"89de6070ffd81198408682cd8b323ee2",
		"abe9392c1553160a85dfd5a3921ac8d6",
		"2768d36f9da0904d7a8784b9c80cd791",
	}
	fakeTarget = []string{"cb0", "325", "7bb", "22e"}
)

// verifyCapSolutions 假上游的**独立**校验：只用上面的预计算常量 + 标准 sha256/hex，
// 不调用被测实现的任何内部函数（capPRNG / capHexPrefixOK 都不用）。
func verifyCapSolutions(token string, solutions []int64) bool {
	if token != fakeChallengeToken || len(solutions) != len(fakeSalt) {
		return false
	}
	for i, salt := range fakeSalt {
		sum := sha256.Sum256([]byte(salt + strconv.FormatInt(solutions[i], 10)))
		if !strings.HasPrefix(hex.EncodeToString(sum[:]), fakeTarget[i]) {
			return false
		}
	}
	return true
}

func checkinAuth(cookie string) *auth.Auth {
	return &auth.Auth{UID: "mc_test", AccessToken: "oma_fake", SigningSecret: "omas_fake", ConsoleCookie: cookie}
}

func mustCookieAuthWithBaizhi() *auth.Auth {
	a := checkinAuth("sess-old")
	a.BaizhiCookie = "baizhi-fake"
	return a
}

// paths 把调用序列压成 `METHOD path` 列表，便于断言「三步顺序」。
func paths(calls []capCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.method+" "+c.path)
	}
	return out
}

// ---------------------------------------------------------------------------
// 成功路径
// ---------------------------------------------------------------------------

// TestCheckinFullFlowClaimed 未签到 → 三步领取成功。
//
// 这是本渠道的主用例：它同时锁死
//   - 三步的**顺序与路径**（多一步/少一步/顺序颠倒都会红）；
//   - challenge 的 body 是 `{}`、redeem 的 body 带 token+solutions；
//   - 第三步的 body **必须**是 `{"captcha_token":"…"}`；
//   - solutions **真的**满足工作量证明（假上游独立校验）。
func TestCheckinFullFlowClaimed(t *testing.T) {
	u := &capUpstream{checkedInBefore: false}
	c := newCapUpstream(t, u)

	rep, err := c.DailyCheckinReport(checkinAuth("sess-1"))
	if err != nil {
		t.Fatalf("未签到时应领取成功，实际报错: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("状态 = %s，期望 %s（msg=%s）", rep.Status, provider.CheckinClaimed, rep.Msg)
	}
	if rep.Amount != 0 {
		t.Errorf("Amount = %d，期望 0（上游回包不含金额，靠差分记账）", rep.Amount)
	}

	want := []string{
		"GET " + epWalletCheckin,
		"POST " + epCaptchaChallenge,
		"POST " + epCaptchaRedeem,
		"POST " + epWalletCheckin,
	}
	got := paths(u.snapshot())
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("调用序列不符\n got: %v\nwant: %v", got, want)
	}

	calls := u.snapshot()
	if calls[1].body != "{}" {
		t.Errorf("challenge body = %q，期望 {}", calls[1].body)
	}
	var redeem struct {
		Token     string  `json:"token"`
		Solutions []int64 `json:"solutions"`
	}
	if err := json.Unmarshal([]byte(calls[2].body), &redeem); err != nil {
		t.Fatalf("redeem body 不是合法 JSON: %v（%s）", err, calls[2].body)
	}
	if redeem.Token != fakeChallengeToken {
		t.Errorf("redeem token = %q，期望 %q", redeem.Token, fakeChallengeToken)
	}
	if len(redeem.Solutions) != fakeC {
		t.Errorf("solutions 长度 = %d，期望 %d", len(redeem.Solutions), fakeC)
	}
	var claim struct {
		CaptchaToken string `json:"captcha_token"`
	}
	if err := json.Unmarshal([]byte(calls[3].body), &claim); err != nil {
		t.Fatalf("checkin body 不是合法 JSON: %v（%s）", err, calls[3].body)
	}
	if claim.CaptchaToken != fakeVerifyToken {
		t.Errorf("captcha_token = %q，期望 %q（必须是 redeem 换来的那一枚）", claim.CaptchaToken, fakeVerifyToken)
	}
	for _, c := range calls {
		if !strings.Contains(c.cookie, CookieNameConsole+"=sess-1") {
			t.Errorf("请求 %s %s 没带控制台 Cookie：%q", c.method, c.path, c.cookie)
		}
	}
}

// TestCheckinAlreadySkipsClaim 已签到：只查状态、**不再领**（幂等短路，省两个请求）。
func TestCheckinAlreadySkipsClaim(t *testing.T) {
	u := &capUpstream{checkedInBefore: true}
	c := newCapUpstream(t, u)

	rep, err := c.DailyCheckinReport(checkinAuth("sess-2"))
	if err != nil {
		t.Fatalf("今日已领不应返回 error: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("状态 = %s，期望 %s", rep.Status, provider.CheckinAlready)
	}
	if got := paths(u.snapshot()); len(got) != 1 || got[0] != "GET "+epWalletCheckin {
		t.Fatalf("已领时不应再打验证码/领取，实际调用: %v", got)
	}
}

// TestCheckinCaptchaRejectionIsRetryable 上游拒绝凭证时，必须如实落成**可重试**的 error。
//
// ⚠️ 命名说明（2026-10-17 独立复核发现）：本用例**不是**字段名的守门人。
// 它的断言是「拿到 10701 就得失败且报文里看得见 10701」——把字段名改错同样满足这个断言，
// 所以对「字段名写错」这种回归它是**不会变红**的。
// **真正锁字段名的是 `TestCheckinFullFlowClaimed`**：它直接断言第三步请求体里的键名与取值
// （`{"captcha_token":"<redeem 换来的那枚>"}`），改名字立即变红。
func TestCheckinCaptchaRejectionIsRetryable(t *testing.T) {
	u := &capUpstream{checkedInBefore: false, requireToken: "only-this-one"}
	c := newCapUpstream(t, u)

	// 假上游只认 "only-this-one"，而实现会提交 redeem 给的 fakeVerifyToken ⇒ 必然 10701。
	rep, err := c.DailyCheckinReport(checkinAuth("sess-3"))
	if err == nil || rep.Status != provider.CheckinError {
		t.Fatalf("字段名/凭证不匹配时必须失败（10701 语义），实际 status=%s err=%v", rep.Status, err)
	}
	if !strings.Contains(rep.Msg, "10701") && !strings.Contains(fmt.Sprint(err), "10701") {
		t.Errorf("失败信息里应能看出是 10701，实际 msg=%q err=%v", rep.Msg, err)
	}
}

// TestCheckinBodyKeyIsExactlyCaptchaToken 直接锁第三步请求体的**键集合**。
//
// 与 FullFlowClaimed 的复合断言互补：这里不关心值，只要求顶层键**恰好**是
// `captcha_token` 一个——任何改名（captcha / token / captchaToken）立即变红。
func TestCheckinBodyKeyIsExactlyCaptchaToken(t *testing.T) {
	u := &capUpstream{checkedInBefore: false}
	c := newCapUpstream(t, u)
	if _, err := c.DailyCheckinReport(checkinAuth("sess-key")); err != nil {
		t.Fatalf("前置领取流程应成功: %v", err)
	}
	calls := u.snapshot()
	body := calls[len(calls)-1].body
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("第三步 body 不是 JSON 对象: %v（%s）", err, body)
	}
	if len(m) != 1 {
		t.Errorf("第三步 body 应只有一个键，实际 %v", m)
	}
	if _, ok := m["captcha_token"]; !ok {
		t.Errorf("第三步 body 的键名必须是 captcha_token，实际 %v", m)
	}
}

// TestCheckinRedeemFailureIsRetryable redeem 失败（如上游改算法）→ 可重试 error，绝不假成功。
func TestCheckinRedeemFailureIsRetryable(t *testing.T) {
	u := &capUpstream{checkedInBefore: false, redeemFail: true}
	c := newCapUpstream(t, u)

	rep, err := c.DailyCheckinReport(checkinAuth("sess-4"))
	if err == nil || rep.Status != provider.CheckinError {
		t.Fatalf("redeem 失败必须落成可重试 error，实际 status=%s err=%v", rep.Status, err)
	}
	if rep.Status.Retryable() != true {
		t.Errorf("error 应可重试")
	}
}

// ---------------------------------------------------------------------------
// 不假成功（旧版最容易犯的错）
// ---------------------------------------------------------------------------

// TestCheckinClaimedRequiresCheckedInTrue code=0 但 checked_in 不是 true ⇒ 不得报 claimed。
func TestCheckinClaimedRequiresCheckedInTrue(t *testing.T) {
	for _, data := range []string{`{"checked_in":false}`, `{}`, `{"checked_in":null}`} {
		u := &capUpstream{checkedInBefore: false, onClaim: func(w http.ResponseWriter) {
			fmt.Fprintf(w, `{"code":0,"message":"success","data":%s}`, data)
		}}
		c := newCapUpstream(t, u)
		rep, err := c.DailyCheckinReport(checkinAuth("sess-5"))
		if rep.Status == provider.CheckinClaimed {
			t.Errorf("data=%s 时不得报 claimed（会假成功）", data)
		}
		if err == nil {
			t.Errorf("data=%s 时应有 error 留痕", data)
		}
	}
}

// TestCheckinShapeDrift 状态回执形状突变 ⇒ 可重试 error，不得当成「未签到」去盲领。
func TestCheckinShapeDrift(t *testing.T) {
	for name, onState := range map[string]func(http.ResponseWriter){
		"缺 checked_in": func(w http.ResponseWriter) { fmt.Fprint(w, `{"code":0,"message":"success","data":{}}`) },
		"data 非对象":     func(w http.ResponseWriter) { fmt.Fprint(w, `{"code":0,"message":"success","data":123}`) },
		"非 JSON":       func(w http.ResponseWriter) { fmt.Fprint(w, `not-json`) },
		"空响应":          func(w http.ResponseWriter) {},
		"业务码非 0":       func(w http.ResponseWriter) { fmt.Fprint(w, `{"code":50003,"message":"internal"}`) },
	} {
		u := &capUpstream{onState: onState}
		c := newCapUpstream(t, u)
		rep, _ := c.DailyCheckinReport(checkinAuth("sess-6"))
		if rep.Status != provider.CheckinError {
			t.Errorf("%s: 状态 = %s，期望 error", name, rep.Status)
		}
		if got := paths(u.snapshot()); len(got) != 1 {
			t.Errorf("%s: 形状不确定时不得继续打领取，实际调用 %v", name, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 传输层与凭据
// ---------------------------------------------------------------------------

// TestCheckinNoCookie 无控制台 Cookie ⇒ 本地判 no_token，且**一个请求都不发**。
func TestCheckinNoCookie(t *testing.T) {
	u := &capUpstream{}
	c := newCapUpstream(t, u)
	rep, err := c.DailyCheckinReport(&auth.Auth{UID: "mc"})
	if err != nil || rep.Status != provider.CheckinNoToken {
		t.Fatalf("期望 no_token/nil，实际 status=%s err=%v", rep.Status, err)
	}
	if n := len(u.snapshot()); n != 0 {
		t.Errorf("无凭据不得发请求，实际发了 %d 个", n)
	}
}

// TestCheckinServerErrorRetryable 5xx ⇒ 可重试 error。
func TestCheckinServerErrorRetryable(t *testing.T) {
	u := &capUpstream{stateStatus: http.StatusBadGateway}
	c := newCapUpstream(t, u)
	rep, err := c.DailyCheckinReport(checkinAuth("sess-7"))
	if err == nil || rep.Status != provider.CheckinError || !rep.Status.Retryable() {
		t.Fatalf("5xx 应可重试，实际 status=%s err=%v", rep.Status, err)
	}
}

// TestCheckinRateLimitIsSoftRate 429 必须分类成软限流（不变量 15：先判 429 再判余额类）。
func TestCheckinRateLimitIsSoftRate(t *testing.T) {
	u := &capUpstream{stateStatus: http.StatusTooManyRequests}
	c := newCapUpstream(t, u)
	_, err := c.DailyCheckinReport(checkinAuth("sess-8"))
	var perr *provider.Error
	if !asProviderError(err, &perr) || perr.Kind != provider.ErrSoftRate {
		t.Fatalf("429 应分类为 ErrSoftRate，实际 err=%v", err)
	}
}

// TestCheckinSessionDeadIsTyped 401 且无法续期 ⇒ 必须**类型化** ErrSessionDead（不变量 19）。
func TestCheckinSessionDeadIsTyped(t *testing.T) {
	u := &capUpstream{stateStatus: http.StatusUnauthorized}
	c := newCapUpstream(t, u)
	_, err := c.DailyCheckinReport(checkinAuth("sess-9")) // 无 baizhiCookie ⇒ 续期必失败
	var perr *provider.Error
	if !asProviderError(err, &perr) || perr.Kind != provider.ErrSessionDead {
		t.Fatalf("401 必须类型化 session dead（裸 error 会让调度器自愈失效），实际 err=%v", err)
	}
}

// TestCheckinRetriesOnceAfterDerive 401 → 派生新控制台会话 → 只重试一次并成功。
//
// 这里只验证「重试后成功」这条主干；派生本身在 client 的测试里覆盖。
func TestCheckinRetriesOnceAfterDerive(t *testing.T) {
	// 第①步先 401，随后正常。用计数器模拟「旧会话失效、派生后可用」。
	var n int
	u := &capUpstream{checkedInBefore: true, onState: func(w http.ResponseWriter) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"code":401,"message":"session expired"}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"message":"success","data":{"checked_in":true}}`)
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == epOAuthAuthorize || strings.Contains(r.URL.Path, "oauth") ||
			strings.Contains(r.URL.Path, "callback") {
			// 让派生失败，从而走不到重试——本用例只断言「401 不会静默变成成功」。
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		u.record(r, "")
		w.Header().Set("Content-Type", "application/json")
		u.onState(w)
	}))
	defer srv.Close()

	c := New()
	c.Console = srv.URL
	rep, err := c.DailyCheckinReport(checkinAuth("sess-10"))
	// 断言：派生不可用时，401 必须落成 error —— 绝不能把「会话失效」静默当成「已领取」。
	if err == nil {
		t.Fatalf("派生不可用时 401 必须落成 error（不得假成功），实际 status=%s", rep.Status)
	}
	if rep.Status == provider.CheckinAlready || rep.Status == provider.CheckinClaimed {
		t.Fatalf("会话失效不得被当成已领/领到，实际 status=%s", rep.Status)
	}
}

// asProviderError 是 errors.As 的薄封装，避免测试文件再引一次 errors。
func asProviderError(err error, target **provider.Error) bool {
	for err != nil {
		if pe, ok := err.(*provider.Error); ok {
			*target = pe
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

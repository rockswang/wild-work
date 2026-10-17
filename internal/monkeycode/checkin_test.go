package monkeycode

// checkin_test.go MonkeyCode 签到（POST {console}/api/v1/users/wallet/checkin）的契约测试。
//
// 全部离线：假上游 httptest，**不打真实上游**（AGENTS §0.1）。
//
// ⚠️ 报文形状是**取证后的最简假设**，不是实测样本（详见 checkin.go 文件头「未确认」段）：
// 信封 `{code,message,data}` 来自同站钱包接口的实测形状；data 内字段名是本实现的
// 「兼容式猜测」——因此下面每组用例都同时覆盖「字段名 A」与「字段名 B」两种形状，
// 就是为了证明**字段名变了不会把失败伪装成成功**。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// fakeCheckin 签到假上游：记录请求，按 handler 回包。
// 返回的 *int32 是命中次数，用于断言「401 时会话续期后只重试一次」这类行为。
type checkinUpstream struct {
	hits      int32
	gotMethod string
	gotPath   string
	gotCookie string
	gotBody   string
}

// newCheckinUpstream 起假上游并把 Client.Console 指过去（签到走控制台域）。
func newCheckinUpstream(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Client, *checkinUpstream) {
	t.Helper()
	rec := &checkinUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rec.hits, 1)
		rec.gotMethod = r.Method
		rec.gotPath = r.URL.Path
		rec.gotCookie = r.Header.Get("Cookie")
		b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		rec.gotBody = string(b)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.Console = srv.URL
	return c, rec
}

// checkinAuth 造一个带控制台会话的账号（签到只认 Cookie，不需要 oma_ key / 签名）。
func checkinAuth(cookie string) *auth.Auth {
	return &auth.Auth{UID: "mc_test", AccessToken: "oma_fake", SigningSecret: "omas_fake", ConsoleCookie: cookie}
}

// okEnvelope 拼 `{code:0,...,data:<data>}` 信封。
func okEnvelope(data string) string {
	return `{"code":0,"message":"success","data":` + data + `}`
}

// ---------------------------------------------------------------------------
// 成功
// ---------------------------------------------------------------------------

// TestCheckinClaimed 领取成功：data 给出本次发放额（100 毫积分）与被加到的余额。
//
// 断言：状态 claimed、Amount 是**换算后的积分**、Msg 含金额、请求形状正确。
func TestCheckinClaimed(t *testing.T) {
	c, rec := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okEnvelope(`{"reward":100,"credits":100,"balance":54487,"already_checked_in":false}`))
	})

	a := checkinAuth("sess-ok")
	rep, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status=%s want claimed（rep=%+v）", rep.Status, rep)
	}
	if rep.Amount != 1 {
		t.Errorf("Amount=%d want 1（100 毫积分 ceil 成 1 积分，与钱包口径一致）", rep.Amount)
	}
	if rep.Msg == "" {
		t.Error("Msg 为空 —— 面板无法区分「真领到」与「活动还没上线」")
	}

	// 请求形状：只有这些是取证到的（路径 + Cookie），其余未确认项不在这里锁死。
	if atomic.LoadInt32(&rec.hits) != 1 {
		t.Errorf("请求次数=%d want 1", rec.hits)
	}
	if rec.gotPath != epWalletCheckin {
		t.Errorf("路径=%q want %q", rec.gotPath, epWalletCheckin)
	}
	if rec.gotMethod != http.MethodPost {
		t.Errorf("方法=%s want POST（见 checkin.go「未确认」①）", rec.gotMethod)
	}
	if rec.gotCookie != CookieNameConsole+"=sess-ok" {
		t.Errorf("Cookie=%q want %s=sess-ok", rec.gotCookie, CookieNameConsole)
	}
}

// TestCheckinClaimedCentsUnit 同样报文，但上游把额度写成**积分整数**（100 → 100 积分）。
// 本实现按钱包口径一律当毫积分处理（ceil），此用例把该决定钉死，防止日后误改口径。
func TestCheckinClaimedCentsUnit(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okEnvelope(`{"reward":125000}`))
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 125 {
		t.Errorf("got %s/%d want claimed/125", rep.Status, rep.Amount)
	}
}

// TestCheckinClaimedNoAmountField 领到但回执里**没有任何金额字段**（本工具对字段名的
// 假设可能过时）：仍是 claimed（不得判失败/丢这一天的积分），Amount=0 ——
// 调度器会降级回快照差分口径。同时带一条 error 留痕，提示形状可能已变更。
func TestCheckinClaimedNoAmountField(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okEnvelope(`{"checked_in_today":false,"balance":54487}`))
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status=%s want claimed（rep=%+v err=%v）", rep.Status, rep, err)
	}
	if rep.Amount != 0 {
		t.Errorf("Amount=%d want 0（上游未回填金额时不得编造）", rep.Amount)
	}
	if err == nil {
		t.Error("金额字段缺失应留一条 error 痕（提示字段名假设可能过时）")
	}
}

// TestCheckinGrantedZeroIsNotClaimed 上游**显式**回填发放额 0（如奖励已被领走 / 达上限）：
// 不得报 claimed —— 那会把「没拿到积分」记成「领到了」。
func TestCheckinGrantedZeroIsNotClaimed(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, okEnvelope(`{"reward":0,"already_checked_in":false}`))
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if rep.Status == provider.CheckinClaimed {
		t.Fatalf("发放额为 0 却报 claimed —— 假成功（rep=%+v）", rep)
	}
	if rep.Status != provider.CheckinError || !rep.Status.Retryable() {
		t.Errorf("got %s want error/retryable", rep.Status)
	}
	if err == nil {
		t.Error("明确 0 发放必须带 error 留痕")
	}
}

// ---------------------------------------------------------------------------
// 今日已领
// ---------------------------------------------------------------------------

// TestCheckinAlready 今日已领的各种可识别形状 → already_claimed，**不是** error。
func TestCheckinAlready(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"data 标记", okEnvelope(`{"already_checked_in":true,"reward":0,"balance":54487}`)},
		{"下发旧发放额", okEnvelope(`{"reward":100,"checked_in_today":true}`)},
		{"业务码 409", `{"code":409,"message":"already checked in","data":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				// 业务错误也可能挂在 HTTP 200 上（同站钱包接口的 code≠0 形态）
				io.WriteString(w, tc.body)
			})
			rep, err := c.DailyCheckinReport(checkinAuth("s"))
			if err != nil {
				t.Fatalf("已领不应返回 error（调度器会因此重试整窗）：%v", err)
			}
			if rep.Status != provider.CheckinAlready {
				t.Fatalf("Status=%s want already_claimed（rep=%+v）", rep.Status, rep)
			}
			if rep.Status.Retryable() {
				t.Error("已领必须视为当日完成，不得继续重试")
			}
			if rep.Msg == "" {
				t.Error("Msg 为空")
			}
		})
	}
}

// TestCheckinAlreadyHTTP409 上游用 HTTP 409 表达已领 → already，且不算错误。
func TestCheckinAlreadyHTTP409(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"code":409,"message":"already checked in"}`)
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if err != nil {
		t.Fatalf("HTTP 409 已领不应返回 error：%v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status=%s want already_claimed", rep.Status)
	}
}

// TestCheckinNotAlreadyInBody 422 带「不代表已领」的 4xx 不得被误判成已领
// （qodercn 的教训：legacy claim 恒 409 被误判成已领 → 假成功、零积分）。
//
// 4xx 的分类错误**照常上抛**（留痕与分类），但 Status 必须是可重试的 error——
// 既不冒充完成，也不冒充领到。
func TestCheckinNotAlreadyInBody(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		io.WriteString(w, `{"code":422,"message":"captcha required"}`)
	})
	rep, _ := c.DailyCheckinReport(checkinAuth("s"))
	if rep.Status == provider.CheckinAlready || rep.Status == provider.CheckinClaimed {
		t.Fatalf("非已领的 4xx 被判成 %s —— 会静默跳过真实领取", rep.Status)
	}
	if rep.Status != provider.CheckinError || !rep.Status.Retryable() {
		t.Fatalf("got %s want error/retryable", rep.Status)
	}
}

// TestCheckinUnknownShapeSignalsTyped 单测「签到失败会带**分类** error」的两种情形，
// 以便在调度器日志里一眼区分「本工具的形状假设过时」与「上游真故障」。
//
// 注意这里用的是「data 整个缺失」与「data 不是对象」；「有 data、只是没有金额字段」
// 走的是另一条分支（claimed + Amount=0 + 留痕 error，见 TestCheckinClaimedNoAmountField）。
func TestCheckinUnknownShapeSignalsTyped(t *testing.T) {
	cases := []struct{ name, body string }{
		{"nodata", `{"code":0,"message":"success"}`},
		{"datascalar", `{"code":0,"message":"success","data":123}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			})
			_, err := c.DailyCheckinReport(checkinAuth("s"))
			if err == nil {
				t.Fatal("want error（形状突变必须留痕）")
			}
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("want 分类过的 *provider.Error，got %T", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 无活动
// ---------------------------------------------------------------------------

// TestCheckinNoCampaign 上游明确表示「没有可领的签到活动」→ no_campaign（可重试，非完成）。
func TestCheckinNoCampaign(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"业务码", http.StatusOK, `{"code":404,"message":"no active checkin campaign","data":null}`},
		{"活动标记", http.StatusOK, okEnvelope(`{"active":false,"reward":0}`)},
		{"HTTP404", http.StatusNotFound, `{"code":404,"message":"no campaign"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			rep, err := c.DailyCheckinReport(checkinAuth("s"))
			if err != nil {
				t.Fatalf("无活动不应返回 error（error 通道无法区分无活动与真错误）：%v", err)
			}
			if rep.Status != provider.CheckinNoCampaign {
				t.Fatalf("Status=%s want no_campaign", rep.Status)
			}
			if !rep.Status.Retryable() {
				t.Error("无活动必须在窗口内重试（活动可能整点后才创建）")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 需要重试（可重试的软失败）
// ---------------------------------------------------------------------------

// TestCheckinServerErrorRetryable 5xx / 网络抖动 → error 状态 + 可重试。
func TestCheckinServerErrorRetryable(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `<html>bad gateway</html>`)
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if rep.Status != provider.CheckinError {
		t.Fatalf("Status=%s want error", rep.Status)
	}
	if !rep.Status.Retryable() {
		t.Error("5xx 必须可重试")
	}
	if err == nil {
		t.Fatal("5xx 必须带 error（留痕/日志）")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrServer {
		t.Fatalf("错误分类 want *provider.Error{ErrServer}，got %v", err)
	}
}

// TestCheckinRateLimitIsSoftRate 429 **优先于**任何余额类判定（AGENTS 不变量 15）：
// 限流 body 常带 quota/exceeded 字样，先判 hardMarker 会误归余额耗尽 → 12h 硬冷却。
func TestCheckinRateLimitIsSoftRate(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"code":429,"message":"quota exceeded, retry later"}`)
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if rep.Status != provider.CheckinError {
		t.Fatalf("Status=%s want error", rep.Status)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("want *provider.Error，got %v", err)
	}
	if pe.Kind != provider.ErrSoftRate {
		t.Fatalf("429 分类=%s want soft_rate（不得被 quota 文案判成 hard_credit）", pe.Kind)
	}
}

// TestCheckinNetworkError 连接层失败（不是 HTTP 状态）→ error 且可重试。
func TestCheckinNetworkError(t *testing.T) {
	c := New()
	c.Console = "http://127.0.0.1:1" // 必然连不上
	c.HTTP = &http.Client{Transport: http.RoundTripper(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}))}
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if err == nil {
		t.Fatal("网络错误必须带 error")
	}
	if rep.Status != provider.CheckinError || !rep.Status.Retryable() {
		t.Fatalf("got %s retryable=%t want error/true", rep.Status, rep.Status.Retryable())
	}
}

// roundTripFunc 让单测注入传输层故障。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ---------------------------------------------------------------------------
// 硬错误：会话失效 / 无凭据
// ---------------------------------------------------------------------------

// TestCheckinSessionDeadIsTyped 401 必须返回**类型化** ErrSessionDead（AGENTS 不变量 19）：
// 调度器用 errors.As 判定自愈，裸 fmt.Errorf 会让刷新重试恒不触发。
func TestCheckinSessionDeadIsTyped(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":401,"message":"session expired"}`)
	})
	rep, err := c.DailyCheckinReport(checkinAuth("stale"))
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("want *provider.Error{ErrSessionDead}，got %v", err)
	}
	if rep.Status != provider.CheckinError {
		t.Errorf("Status=%s want error（调度器据 error 类型走续期）", rep.Status)
	}
}

// TestCheckinNoConsoleCookie 账号没有控制台会话：本地直接判 no_token，**不发请求**。
// 老凭据（只导入了 oma_ key）就是这种情形 —— 不能靠签到去打上游换一个 401。
func TestCheckinNoConsoleCookie(t *testing.T) {
	c, rec := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("无 Cookie 时不得发请求")
	})
	rep, err := c.DailyCheckinReport(&auth.Auth{UID: "mc", AccessToken: "oma_k"})
	if err != nil {
		t.Fatalf("无凭据不应是「错误」，而是 no_token 状态：%v", err)
	}
	if rep.Status != provider.CheckinNoToken {
		t.Fatalf("Status=%s want no_token", rep.Status)
	}
	if rep.Msg == "" {
		t.Error("Msg 应提示用户重新导入客户端凭据")
	}
	if atomic.LoadInt32(&rec.hits) != 0 {
		t.Errorf("请求次数=%d want 0", rec.hits)
	}
}

// ---------------------------------------------------------------------------
// 响应体异常
// ---------------------------------------------------------------------------

// TestCheckinMalformedBody 解析不了的响应体不得报成功。
func TestCheckinMalformedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非 JSON", `not-json`},
		{"空 body", ``},
		{"data 类型错", `{"code":0,"data":"oops"}`},
		{"reward 是字符串", okEnvelope(`{"reward":"a lot"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			})
			rep, err := c.DailyCheckinReport(checkinAuth("s"))
			if rep.Status == provider.CheckinClaimed || rep.Status == provider.CheckinAlready {
				t.Fatalf("异常响应被判成 %s —— 假成功（rep=%+v err=%v）", rep.Status, rep, err)
			}
			if err == nil {
				t.Fatal("异常响应必须带 error 留痕")
			}
		})
	}
}

// TestCheckinMissingDataField 信封 code=0 但**整个 data 缺失**：形状突变，
// 不得当成「已领」也不得当成「领到」，按可重试错误处理。
func TestCheckinMissingDataField(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"message":"success"}`)
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if err == nil {
		t.Fatal("data 缺失必须带 error（形状突变要能被看见）")
	}
	if rep.Status != provider.CheckinError || !rep.Status.Retryable() {
		t.Fatalf("got %s want error/retryable", rep.Status)
	}
}

// TestCheckinUnexpectedBusinessCode 未知业务码不得当成功。
func TestCheckinUnexpectedBusinessCode(t *testing.T) {
	c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":50003,"message":"internal"}`)
	})
	rep, err := c.DailyCheckinReport(checkinAuth("s"))
	if rep.Status == provider.CheckinClaimed {
		t.Fatal("未知业务码被判 claimed")
	}
	if err == nil {
		t.Fatal("未知业务码必须带 error")
	}
}

// ---------------------------------------------------------------------------
// 会话续期
// ---------------------------------------------------------------------------

// TestCheckinRetriesOnceAfterDerive 控制台会话失效（401）→ 用百智云会话现派生一份 →
// **只重试一次**（签到是会发积分写操作，不得狂重试）。
func TestCheckinRetriesOnceAfterDerive(t *testing.T) {
	var checkinHits, deriveHits int32
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case oauthCallbackPath:
			atomic.AddInt32(&deriveHits, 1)
			http.SetCookie(w, &http.Cookie{Name: CookieNameConsole, Value: "fresh", Path: "/"})
			w.WriteHeader(http.StatusFound)
		case epWalletCheckin:
			atomic.AddInt32(&checkinHits, 1)
			if r.Header.Get("Cookie") != CookieNameConsole+"=fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, okEnvelope(`{"reward":100}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer console.Close()
	baizhi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Location", q.Get("redirect_uri")+"?code=C&state="+q.Get("state"))
		w.WriteHeader(http.StatusFound)
	}))
	defer baizhi.Close()

	c := New()
	c.Console = console.URL
	c.Baizhi = baizhi.URL
	a := &auth.Auth{UID: "mc_test", ConsoleCookie: "stale", BaizhiCookie: "bz"}

	rep, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("续期后应成功：%v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status=%s want claimed", rep.Status)
	}
	if n := atomic.LoadInt32(&checkinHits); n != 2 {
		t.Errorf("签到请求次数=%d want 2（原会话 + 续期后各一次）", n)
	}
	if n := atomic.LoadInt32(&deriveHits); n != 1 {
		t.Errorf("派生次数=%d want 1", n)
	}
	if got := consoleCookieOf(a); got != "fresh" {
		t.Errorf("续期得到的新会话未写回账号：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 接口与旧通道
// ---------------------------------------------------------------------------

// TestDailyCheckinWrapper claimed/already → nil；其余可重试状态 → error。
func TestDailyCheckinWrapper(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		status   int
		wantErr  bool
		upstream string
	}{
		{"claimed", okEnvelope(`{"reward":100}`), http.StatusOK, false, ""},
		{"already", okEnvelope(`{"already_checked_in":true}`), http.StatusOK, false, ""},
		{"no_campaign", `{"code":404,"message":"no campaign"}`, http.StatusNotFound, true, ""},
		{"server", `{}`, http.StatusInternalServerError, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newCheckinUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			err := c.DailyCheckin(checkinAuth("s"))
			if tc.wantErr && err == nil {
				t.Fatal("want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want nil，got %v", err)
			}
		})
	}
}

// TestClientImplementsCheckinInterfaces 编译期契约：调度器只认这两个接口。
func TestClientImplementsCheckinInterfaces(t *testing.T) {
	var _ provider.Upstream = New()
	var _ provider.CheckinReporter = New()
}

// TestCheckinPayloadShape 体积守门：假上游的回包样本必须能被 json.Unmarshal 进
// 本实现的信封结构（防止有人改字段名却忘了改测试样本，两边一起错）。
func TestCheckinPayloadShape(t *testing.T) {
	var env checkinEnvelope
	raw := okEnvelope(`{"reward":100,"balance":1,"already_checked_in":true}`)
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("信封解析失败: %v", err)
	}
	if env.Code != 0 || len(env.Data) == 0 {
		t.Fatalf("信封字段异常：%+v", env)
	}
}

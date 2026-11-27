package qodercn

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

const (
	testTicket = "TICKET-SECRET-8f3c"
	testCookie = "qoder_session_cookie=COOKIE-SECRET-4b91"
)

// webUpstream 假 qoder.cn：业务额度端点 + 换票三步链路，全部计数便于断言缓存/重试。
type webUpstream struct {
	mu sync.Mutex

	transfers    int
	hops         int
	redirected   int
	credits      int
	transfer500  bool
	noCookieHop  bool // 第一轮 303 不带 Set-Cookie
	rejectOnce   bool // 第一轮积分包接口回 401
	creditBodies []string
	ticketUsed   map[string]bool

	lastTransferAuth string
	lastTargetURL    string
	lastCookie       string
	lastCSRF         string
	lastXHR          string
	lastReferer      string
}

func (u *webUpstream) handler(srvURL string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(EpQuotaUsage, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"userQuota":{"total":1000,"used":200,"remaining":800},`+
			`"addOnQuota":{"total":500,"used":100,"remaining":400}}`)
	})
	mux.HandleFunc(EpWebSessionTransfers, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.transfers++
		if u.ticketUsed == nil {
			u.ticketUsed = map[string]bool{}
		}
		u.lastTransferAuth = r.Header.Get("Authorization")
		var body struct {
			TargetURL string `json:"targetUrl"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.lastTargetURL = body.TargetURL
		u.mu.Unlock()
		if u.transfer500 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// 真实网关每次换票发新 ticket；桩也按轮次发，才能验证「重换一次」。
		u.mu.Lock()
		seq := u.transfers
		u.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{
			"transferUrl": srvURL + "/sso/transfer?ticket=" + testTicket + "-" + fmt.Sprint(seq),
		})
	})
	mux.HandleFunc("/sso/transfer", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hops++
		ticket := r.URL.Query().Get("ticket")
		if u.ticketUsed == nil {
			u.ticketUsed = map[string]bool{}
		}
		used := u.ticketUsed[ticket]
		u.ticketUsed[ticket] = true
		skipCookie := u.noCookieHop && u.hops == 1
		u.mu.Unlock()
		if used {
			// 一次性 ticket 被复用说明缓存失效，必须能被测试发现。
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !skipCookie {
			w.Header().Add("Set-Cookie", testCookie+"; Path=/; HttpOnly")
			w.Header().Add("Set-Cookie", "theme=light; Path=/")
		}
		w.Header().Set("Location", srvURL+"/account/usage")
		w.WriteHeader(http.StatusSeeOther)
	})
	mux.HandleFunc("/account/usage", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.redirected++
		u.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc(EpWebCredits, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.credits++
		u.lastCookie = r.Header.Get("Cookie")
		u.lastCSRF = r.Header.Get("x-csrf-token")
		u.lastXHR = r.Header.Get("x-requested-with")
		u.lastReferer = r.Header.Get("Referer")
		reject := u.rejectOnce && u.credits == 1
		body := `{
			"resource_package_quota":{"quota_detail":[
				{"remaining_value":0,"expires_at":1735689600000},
				{"remaining_value":50,"expires_at":0},
				{"remaining_value":100,"expires_at":1763568000000},
				{"remaining_value":30,"expires_at":1763654400000},
				{"remaining_value":200,"expires_at":1763827200000}
			]},
			"add_on_quota":{"quota_detail":[
				{"remaining_value":"20","expires_at":"1763740800000"}
			]},
			"total_quota":{"quota_detail":[
				{"remaining_value":400,"expires_at":1763568000000}
			]},
			"ignored_scalar": 42
		}`
		if len(u.creditBodies) > 0 {
			idx := u.credits - 1
			if idx >= len(u.creditBodies) {
				idx = len(u.creditBodies) - 1
			}
			body = u.creditBodies[idx]
		}
		u.mu.Unlock()
		if reject {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, body)
	})
	return mux
}

func newWebTestClient(t *testing.T, u *webUpstream) (*Client, *httptest.Server) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 延迟到 srv 建好后再构造 handler，transferUrl 需要本机地址。
		u.handler(srv.URL).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	// 手工注入业务/网页基址与假上游 client，避免测试打真实 qoder.cn。
	c := New()
	c.Base = srv.URL
	c.Gateway = srv.URL
	c.Web = srv.URL
	c.HTTP = srv.Client()
	return c, srv
}

func webTestAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "dt-test-device-token", RefreshToken: "drt-test"}
}

// 最早有效条目 = 1763568000000 ms = 2026-11-20 00:00:00+08:00（UTC 11-19 16:00）。
const wantExpiryUnix = int64(1763568000)

func TestParseWebCreditPacks(t *testing.T) {
	packs := parseWebCreditPacks([]byte(`{
		"resource_package_quota":{"quota_detail":[
			{"remaining_value":0,"expires_at":1000},
			{"remaining_value":10,"limit_value":30,"used_value":20,"expires_at":0},
			{"remaining_value":10,"expires_at":3000},
			{"remaining_value":10,"expires_at":2000}
		]},
		"other_quota":{"quota_detail":[{"remaining_value":"5","expires_at":"2500"}]},
		"total_quota":{"quota_detail":[{"remaining_value":99,"expires_at":1500}]},
		"no_detail":{"x":1}
	}`))
	if len(packs) != 4 {
		t.Fatalf("积分包数 = %d，期望 4（含不过期包）", len(packs))
	}
	var remain, earliest int64
	var foundTotals bool
	for _, p := range packs {
		remain += p.remain
		if p.expiresAt > 0 && (earliest == 0 || p.expiresAt < earliest) {
			earliest = p.expiresAt
		}
		if p.total == 30 && p.used == 20 {
			foundTotals = true
		}
	}
	if remain != 35 || earliest != 2000 || !foundTotals {
		t.Fatalf("解析结果 remain=%d earliest=%d totals=%v", remain, earliest, foundTotals)
	}
	wrapped := parseWebCreditPacks([]byte(`{"data":{"resource_package_quota":{"quota_detail":[{"remaining_value":3,"expires_at":4500}]}}}`))
	if len(wrapped) != 1 || wrapped[0].expiresAt != 4500 {
		t.Errorf("data 包裹形态 = %+v", wrapped)
	}
	for _, bad := range []string{`not-json`, `{}`, `{"p":{"quota_detail":[{"remaining_value":0,"expires_at":1000}]}}`} {
		if got := parseWebCreditPacks([]byte(bad)); len(got) != 0 {
			t.Errorf("%q 应返回空列表，得到 %+v", bad, got)
		}
	}
}

func TestWebExpireAtString(t *testing.T) {
	if s := webExpireAtString(time.Time{}); s != "" {
		t.Fatalf("零值应返回空串，得到 %q", s)
	}
	s := webExpireAtString(time.Unix(wantExpiryUnix, 0))
	if !strings.HasSuffix(s, "+08:00") {
		t.Fatalf("应为 UTC+8 的 RFC3339，得到 %q", s)
	}
	parsed, ok := provider.ExpireInstant(s)
	if !ok || parsed.Unix() != wantExpiryUnix {
		t.Fatalf("回读失败：%q ok=%v unix=%d", s, ok, parsed.Unix())
	}
}

func TestUserResourceDetailWebExpiryChain(t *testing.T) {
	u := &webUpstream{}
	c, srv := newWebTestClient(t, u)
	logBuf := captureLogs(t)

	total, items, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatalf("余额读取不应失败：%v", err)
	}
	if total != 1200 {
		t.Fatalf("total=%d，期望 1200", total)
	}
	now := time.Unix(wantExpiryUnix-3600, 0)
	if usable, _ := provider.Summarize(items); usable != total {
		t.Fatalf("逐包可用余额=%d，与总额 %d 不一致", usable, total)
	}
	if got := provider.ExpiringWithinDays(items, 1, now); got != 100 {
		t.Fatalf("今日临期=%d，期望只统计今日到期的 100，而不是整个赠送池", got)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.transfers != 1 || u.hops != 1 || u.credits != 1 {
		t.Fatalf("链路次数 transfers=%d hops=%d credits=%d，期望各 1", u.transfers, u.hops, u.credits)
	}
	if u.redirected != 0 {
		t.Fatal("303 被跟随了，cookie 会被吃掉")
	}
	if u.lastTransferAuth != "Bearer dt-test-device-token" {
		t.Errorf("换票 Authorization = %q", u.lastTransferAuth)
	}
	if u.lastTargetURL != srv.URL+WebUsageTarget {
		t.Errorf("targetUrl = %q", u.lastTargetURL)
	}
	if !strings.Contains(u.lastCookie, "qoder_session_cookie=COOKIE-SECRET-4b91") ||
		!strings.Contains(u.lastCookie, "theme=light") {
		t.Errorf("Cookie 拼接不对：%q", u.lastCookie)
	}
	if u.lastCSRF != webCSRFHeader || u.lastXHR != "XMLHttpRequest" ||
		u.lastReferer != srv.URL+WebUsageTarget {
		t.Errorf("积分包头不对 csrf=%q xhr=%q referer=%q", u.lastCSRF, u.lastXHR, u.lastReferer)
	}

	var expireAt string
	for _, it := range items {
		if it.ExpireAt != "" {
			expireAt = it.ExpireAt
		}
	}
	if expireAt == "" {
		t.Fatal("ExpireAt 未填写")
	}
	earliest, ok := provider.EarliestExpire(items, time.Unix(wantExpiryUnix-3600, 0))
	if !ok || earliest.Unix() != wantExpiryUnix {
		t.Fatalf("EarliestExpire = %v ok=%v，期望 unix=%d", earliest, ok, wantExpiryUnix)
	}
	if strings.Contains(logBuf.String(), testTicket) || strings.Contains(logBuf.String(), "COOKIE-SECRET-4b91") {
		t.Fatal("ticket 或 cookie 落进了日志")
	}
}

func TestWebCreditPacksKeepResidualWithoutExpiry(t *testing.T) {
	u := &webUpstream{creditBodies: []string{
		`{"resource_package_quota":{"quota_detail":[{"remaining_value":399,"limit_value":400,"used_value":1,"expires_at":1763568000000}]}}`,
	}}
	c, _ := newWebTestClient(t, u)
	total, items, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	if usable, _ := provider.Summarize(items); usable != total {
		t.Fatalf("逐包可用余额=%d，与总额 %d 不一致", usable, total)
	}
	if got := provider.ExpiringWithinDays(items, 1, time.Unix(wantExpiryUnix-3600, 0)); got != 399 {
		t.Fatalf("临期=%d，期望保留有到期日的 399", got)
	}
	var residual *provider.ResourceItem
	for i := range items {
		if items[i].Key == "addOnQuota:other" {
			residual = &items[i]
		}
	}
	if residual == nil || residual.Remain != 1 || residual.ExpireAt != "" {
		t.Fatalf("余额差应保留为无到期残差：%+v", residual)
	}
}

func TestWebExpiryReusesCookie(t *testing.T) {
	u := &webUpstream{}
	c, _ := newWebTestClient(t, u)
	for i := 0; i < 3; i++ {
		if _, _, err := c.UserResourceDetail(webTestAuth()); err != nil {
			t.Fatalf("第 %d 次读取失败：%v", i+1, err)
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.transfers != 1 || u.hops != 1 {
		t.Fatalf("复用 cookie 时不应再换票：transfers=%d hops=%d", u.transfers, u.hops)
	}
	if u.credits != 3 {
		t.Fatalf("每次刷新都必须重读积分包：credits=%d", u.credits)
	}
}

func TestWebCreditPacksRefreshWithinCookieLifetime(t *testing.T) {
	u := &webUpstream{creditBodies: []string{
		`{"resource_package_quota":{"quota_detail":[{"remaining_value":400,"expires_at":1763654400000}]}}`,
		`{"resource_package_quota":{"quota_detail":[{"remaining_value":35,"total_value":100,"used_value":65,"expires_at":1763568000000},{"remaining_value":365,"expires_at":1763740800000}]}}`,
	}}
	c, _ := newWebTestClient(t, u)
	now := time.Unix(wantExpiryUnix-3600, 0)

	_, first, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := provider.EarliestExpire(first, now); !ok || got.Unix() != wantExpiryUnix+86400 {
		t.Fatalf("首次最早到期 = %v ok=%v", got, ok)
	}
	_, second, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.ExpiringWithinDays(second, 1, now); got != 35 {
		t.Fatalf("刷新后临期=%d，期望签到包实际剩余 35（已用 65）", got)
	}
	if got, ok := provider.EarliestExpire(second, now); !ok || got.Unix() != wantExpiryUnix {
		t.Fatalf("刷新后最早到期 = %v ok=%v，期望更新到新签到包", got, ok)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.transfers != 1 || u.hops != 1 || u.credits != 2 {
		t.Fatalf("transfers=%d hops=%d credits=%d", u.transfers, u.hops, u.credits)
	}
}

func TestWebExpiryReexchangesOnUnauthorized(t *testing.T) {
	u := &webUpstream{rejectOnce: true}
	c, _ := newWebTestClient(t, u)
	_, items, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatalf("401 重换后仍不该让余额读取失败：%v", err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.transfers != 2 || u.hops != 2 || u.credits != 2 {
		t.Fatalf("应重换一次票：transfers=%d hops=%d credits=%d", u.transfers, u.hops, u.credits)
	}
	if len(u.ticketUsed) != 2 {
		t.Fatalf("第二轮必须用新 ticket，实际不同 ticket 数=%d", len(u.ticketUsed))
	}
	if !hasExpiry(items) {
		t.Fatal("重换成功后应拿到 ExpireAt")
	}
}

func TestWebExpiryRetriesWhenHopHasNoCookie(t *testing.T) {
	u := &webUpstream{noCookieHop: true}
	c, _ := newWebTestClient(t, u)
	_, items, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatal(err)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.transfers != 2 || u.hops != 2 || u.credits != 1 {
		t.Fatalf("303 无 cookie 应重换一次：transfers=%d hops=%d credits=%d", u.transfers, u.hops, u.credits)
	}
	if !hasExpiry(items) {
		t.Fatal("第二轮应拿到 ExpireAt")
	}
}

func TestWebExpiryDegradesOnFailure(t *testing.T) {
	u := &webUpstream{transfer500: true}
	c, _ := newWebTestClient(t, u)
	logBuf := captureLogs(t)
	total, items, err := c.UserResourceDetail(webTestAuth())
	if err != nil {
		t.Fatalf("网页接口挂掉不能让余额读取失败：%v", err)
	}
	if total != 1200 || len(items) != 2 {
		t.Fatalf("降级后条目应原样返回：total=%d items=%d", total, len(items))
	}
	if hasExpiry(items) {
		t.Fatal("降级时 ExpireAt 必须留空")
	}
	u.mu.Lock()
	transfers := u.transfers
	u.mu.Unlock()
	if transfers != 2 {
		t.Fatalf("换票失败最多重试一次，实际 %d 次", transfers)
	}
	if strings.Contains(logBuf.String(), testTicket) || strings.Contains(logBuf.String(), "COOKIE-SECRET-4b91") {
		t.Fatal("失败日志里出现 ticket 或 cookie")
	}
}

func hasExpiry(items []provider.ResourceItem) bool {
	for _, it := range items {
		if it.ExpireAt != "" {
			return true
		}
	}
	return false
}

// captureLogs 在测试期间接管标准 logger；t.Cleanup 恢复。并行测试不可用。
func captureLogs(t *testing.T) *strings.Builder {
	t.Helper()
	buf := &strings.Builder{}
	old := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(old)
		log.SetFlags(oldFlags)
	})
	return buf
}

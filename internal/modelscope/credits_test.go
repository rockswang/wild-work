package modelscope

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 探针实测的逐字报文（.scratch/modelscope-credits/probe-findings.md §1/§3）：
// balance 是**小写**信封（success/data），rate-limit 是**首字母大写**的旧式信封
// （Code/Data/Message/Success）——两者必须各解析一份。
const (
	fakeBalanceBody = `{"success":true,"request_id":"req-1","data":{"total_balance":350,"available_balance":350,"frozen_amount":0}}`
	fakeUsageBody   = `{"Code":200,"Data":{"currentUsagePerDay":1234},"Message":"success","RequestId":"req-2","Success":true}`
)

// siteObs 记录假主站收到的头（断言鉴权形态与零 token 落盘）。
type siteObs struct {
	auth   []string
	cookie []string
	ua     []string
}

// fakeSite 假主站：按端点返回可覆盖的状态码/报文，并记录收到的请求头。
type fakeSite struct {
	balanceStatus int
	balanceBody   string
	usageStatus   int
	usageBody     string
	obs           siteObs
}

func (f *fakeSite) write(w http.ResponseWriter, status int, body string) {
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fakeSite) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(EpMagicubesBalance, func(w http.ResponseWriter, r *http.Request) {
		f.obs.auth = append(f.obs.auth, r.Header.Get("Authorization"))
		f.obs.ua = append(f.obs.ua, r.Header.Get("User-Agent"))
		f.write(w, f.balanceStatus, f.balanceBody)
	})
	mux.HandleFunc(EpDailyUsage, func(w http.ResponseWriter, r *http.Request) {
		f.obs.cookie = append(f.obs.cookie, r.Header.Get("Cookie"))
		f.write(w, f.usageStatus, f.usageBody)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newSiteClient 构造指向假主站的客户端（SiteBase 注入，Kind 仍决定区域语义）。
func newSiteClient(kind provider.Kind, srvURL, tok string) *Client {
	c := New(kind)
	c.SiteBase = srvURL
	c.Base = srvURL
	if tok != "" {
		c.HTTP = &http.Client{}
	}
	return c
}

// TestUserResourceDetailMapsBalance D2 额度映射：remain == available_balance（不含冻结），
// total 是面额、used = total − available − frozen，且两个端点各用**各自**的鉴权头。
func TestUserResourceDetailMapsBalance(t *testing.T) {
	f := &fakeSite{balanceBody: fakeBalanceBody, usageBody: fakeUsageBody}
	srv := f.start(t)
	const tok = "ms-test-token"
	c := newSiteClient(provider.ModelScope, srv.URL, tok)

	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: tok})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 350 {
		t.Fatalf("remain = %d，期望 350（= available_balance）", remain)
	}
	usable, unusable := provider.Summarize(items)
	if usable != 350 || unusable != 0 {
		t.Fatalf("Summarize = (%d, %d)，期望 (350, 0)", usable, unusable)
	}
	var main, usage *provider.ResourceItem
	for i := range items {
		switch items[i].Name {
		case itemNameUsable:
			main = &items[i]
		case itemNameDailyUsage:
			usage = &items[i]
		}
	}
	if main == nil {
		t.Fatalf("缺 %q 条目：%+v", itemNameUsable, items)
	}
	if main.Remain != 350 || main.Total != 350 || main.Used != 0 || !main.Usable || main.InfoOnly {
		t.Errorf("可用魔粒条目口径错误：%+v", *main)
	}
	if main.Key != "" {
		t.Errorf("可用魔粒不得设 Key（伪键必须保持 Name，见 itemNameUsable 注释）：%q", main.Key)
	}
	if usage == nil {
		t.Fatalf("缺 %q 条目：%+v", itemNameDailyUsage, items)
	}
	// 今日用量：InfoOnly + Unit=token，且**不改变 remain、不进任何小计**。
	if !usage.InfoOnly || usage.Unit != "token" || !usage.Usable {
		t.Errorf("今日用量条目应为 InfoOnly/Unit=token/Usable：%+v", *usage)
	}
	if usage.Used != 1234 {
		t.Errorf("今日用量 = %d，期望 1234", usage.Used)
	}
	if len(items) != 2 {
		t.Errorf("frozen=0 不应铺第三条明细：%+v", items)
	}
	// 鉴权形态：balance 走 Bearer、rate-limit 走 Cookie（D7 逐字实测）。
	if len(f.obs.auth) != 1 || f.obs.auth[0] != "Bearer "+tok {
		t.Errorf("balance 鉴权头 = %v，期望 [Bearer <token>]", f.obs.auth)
	}
	if len(f.obs.cookie) != 1 || f.obs.cookie[0] != "m_session_id="+tok {
		t.Errorf("rate-limit 鉴权头 = %v，期望 [m_session_id=<token>]", f.obs.cookie)
	}
	for _, ua := range f.obs.ua {
		if !strings.Contains(ua, "Mozilla/5.0") {
			t.Errorf("主站请求应带浏览器 UA（探针口径），实得 %q", ua)
		}
	}
}

// TestFrozenIsUnusable 冻结魔粒：单列一条 Usable=false 的明细（进「不可用」小计），
// **不进** remain、也不进可用小计（与 TraeWork 专用池同款）。
func TestFrozenIsUnusable(t *testing.T) {
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":500,"available_balance":300,"frozen_amount":50}}`,
		usageBody:   fakeUsageBody,
	}
	c := newSiteClient(provider.ModelScopeAI, f.start(t).URL, "ms-x")

	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 300 {
		t.Fatalf("remain = %d，期望 300（available，不含冻结）", remain)
	}
	usable, unusable := provider.Summarize(items)
	if usable != 300 {
		t.Errorf("可用小计 = %d，期望 300", usable)
	}
	if unusable != 50 {
		t.Errorf("不可用小计 = %d，期望 50（冻结）", unusable)
	}
	var frozen *provider.ResourceItem
	for i := range items {
		if items[i].Name == itemNameFrozen {
			frozen = &items[i]
		}
	}
	if frozen == nil {
		t.Fatalf("缺 %q 条目：%+v", itemNameFrozen, items)
	}
	if frozen.Usable || frozen.InfoOnly || frozen.Remain != 50 {
		t.Errorf("冻结条目应为 Usable=false、非 InfoOnly、Remain=50：%+v", *frozen)
	}
	var main *provider.ResourceItem
	for i := range items {
		if items[i].Name == itemNameUsable {
			main = &items[i]
		}
	}
	if main == nil || main.Used != 150 {
		t.Errorf("used 应为 total−available−frozen=150：%+v", main)
	}
}

// TestUsedClampedToZero 反例护栏：上游字段自相矛盾（available+frozen > total）时
// used 必须夹到 0，不能让负的「已用」流到面板。
func TestUsedClampedToZero(t *testing.T) {
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":100,"available_balance":120,"frozen_amount":30}}`,
		usageBody:   fakeUsageBody,
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	_, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	for _, it := range items {
		if it.Name == itemNameUsable && it.Used != 0 {
			t.Errorf("used 应夹到 0，实得 %d", it.Used)
		}
	}
}

// TestUserResourceMatchesDetail D7：UserResource 直接复用 UserResourceDetail 的 remain，
// 两者必须**逐位一致**（各算一份必然漂移）。
func TestUserResourceMatchesDetail(t *testing.T) {
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":777,"available_balance":512,"frozen_amount":9}}`,
		usageBody:   `{"Code":200,"Data":{"currentUsagePerDay":88},"Success":true}`,
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	remain, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	one, err := c.UserResource(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if one != remain || one != 512 {
		t.Fatalf("UserResource=%d / UserResourceDetail=%d，期望同为 512", one, remain)
	}
}

// TestDailyUsageFailureDegrades 今日用量是附加维度：它失败**不得**让余额刷新整体失败
// （否则一个可选维度会把面板主数字打成「待刷新」）。
func TestDailyUsageFailureDegrades(t *testing.T) {
	f := &fakeSite{
		balanceBody: fakeBalanceBody,
		usageStatus: http.StatusInternalServerError,
		usageBody:   `{"Code":500,"Message":"boom","Success":false}`,
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	logs := captureLog(t)
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("用量失败不应让余额刷新失败：%v", err)
	}
	if remain != 350 {
		t.Fatalf("remain = %d，期望 350", remain)
	}
	for _, it := range items {
		if it.Name == itemNameDailyUsage {
			t.Fatalf("用量读取失败时不应产出今日用量条目：%+v", it)
		}
	}
	if !strings.Contains(logs.String(), "今日用量读取失败") {
		t.Errorf("降级必须留日志（否则尾巴没了），实际：%q", logs.String())
	}
}

// TestBalanceUnauthorizedIsSessionDead 401/403 = 凭据失效 → **显式** ErrSessionDead
// （可自愈标记：面板「需重新登录」+ 重贴后按 auth 文件 mtime 自动清除），
// 绝不是账号级惩罚（不 Disable、不 Cooldown、不做自动 refresh）。
func TestBalanceUnauthorizedIsSessionDead(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f := &fakeSite{balanceStatus: status, balanceBody: `{"success":false,"code":"InvalidAuthentication","message":"Invalid authentication: authentication required"}`}
		c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
		_, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
		var pe *provider.Error
		if !errors.As(err, &pe) {
			t.Fatalf("status=%d: 应返回 *provider.Error，实得 %v", status, err)
		}
		if pe.Kind != provider.ErrSessionDead {
			t.Errorf("status=%d: Kind = %s，期望 ErrSessionDead", status, pe.Kind)
		}
		if pe.Status != status {
			t.Errorf("status=%d: Status = %d，期望原样保留", status, pe.Status)
		}
		// 注：ErrSessionDead 在 provider.Rotatable() 里为真（换到别的号继续服务本请求），
		// 这是全仓统一语义；面板惩罚侧只做**可自愈标记**（handler 路径），
		// 不 Cool down / 不 Disable——本渠道 RefreshToken 为空实现，也不会做自动 refresh。
	}
}

// TestBalanceServerErrorIsTyped 5xx 必须是**类型化**错误（不变式 19：自愈链靠 errors.As
// 取 Kind），且不得被误归成 SessionDead（否则一次上游抖动会弹「需重新登录」）。
func TestBalanceServerErrorIsTyped(t *testing.T) {
	f := &fakeSite{balanceStatus: http.StatusServiceUnavailable, balanceBody: `{"success":false,"message":"gateway down"}`}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	_, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("应返回 *provider.Error，实得 %v", err)
	}
	if pe.Kind == provider.ErrSessionDead {
		t.Errorf("5xx 不得归成 ErrSessionDead")
	}
	if pe.Kind != provider.ErrServer {
		t.Errorf("Kind = %s，期望 ErrServer", pe.Kind)
	}
}

// TestBalanceBusinessErrorIsNotZero 信封 success=false（HTTP 200）时报错而不是静默回 0
// ——回 0 会被面板读成「额度已用光」。
func TestBalanceBusinessErrorIsNotZero(t *testing.T) {
	f := &fakeSite{balanceBody: `{"success":false,"code":"SomeBusinessError","message":"nope"}`}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	remain, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err == nil {
		t.Fatalf("业务失败应报错，却回了 remain=%d", remain)
	}
	if remain != 0 {
		t.Errorf("失败时不得回非零 remain：%d", remain)
	}
}

// TestBalanceErrorScrubsToken 零 token 落盘（不变量 2）：上游若把凭据回显在错误体里，
// 错误消息里必须已抹掉，绝不流进日志/面板。
func TestBalanceErrorScrubsToken(t *testing.T) {
	const secret = "ms-0123456789abcdef0123456789abcdef"
	f := &fakeSite{
		balanceStatus: http.StatusForbidden,
		balanceBody:   fmt.Sprintf(`{"success":false,"message":"token %s rejected"}`, secret),
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, secret)
	_, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: secret})
	if err == nil {
		t.Fatal("403 应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息泄露 token：%v", err)
	}
}

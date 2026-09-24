package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newPanelApp 构造一个带单渠道、单账号的最小 App，用于面板鉴权回归测试。
func newPanelApp(t *testing.T, listenHost, adminPass string) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cfg.Listen = config.Listen{Host: listenHost, Port: 7863}
	cfg.AdminPass = adminPass

	p := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	p.Add(&auth.Auth{Kind: "workbuddy", UID: "wb-1", AccessToken: "t", RefreshToken: "r"})
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: &fakeUpstream{remain: 10}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// panelMux 装配管理 API 路由（等价于 main.go 里的 AttachAPI）。
func panelMux(a *App) *http.ServeMux {
	mux := http.NewServeMux()
	a.HandleAPI(mux)
	return mux
}

func doReq(mux *http.ServeMux, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "192.168.1.50:12345"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// TestPanelNoAuthWhenNoPassword 未设置密码（仅本机监听）时，管理 API 不鉴权。
func TestPanelNoAuthWhenNoPassword(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)
	if got := doReq(mux, "GET", "/api/state", "", nil).Code; got != 200 {
		t.Fatalf("无密码时 /api/state 应 200，实际 %d", got)
	}
	if got := doReq(mux, "POST", "/api/quit", "", nil).Code; got != 200 {
		t.Fatalf("无密码时 /api/quit 应 200，实际 %d", got)
	}
}

// TestPanelRequiresSession 设置密码后：匿名 401（且无需触发 quit 等副作用），登录后可访问。
func TestPanelRequiresSession(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "s3cret-pass")
	mux := panelMux(a)

	// 匿名访问受保护端点：401 + need_login（避免测试真的退出程序：用 /api/logs 而非 /api/quit）
	w := doReq(mux, "GET", "/api/logs", "", nil)
	if w.Code != 401 {
		t.Fatalf("匿名访问应 401，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "need_login") {
		t.Errorf("401 响应应带 need_login，实际 %s", w.Body.String())
	}

	// 会话探针不返回 401，供前端首次加载判断
	probe := doReq(mux, "GET", "/api/auth/state", "", nil)
	if probe.Code != 200 {
		t.Fatalf("/api/auth/state 应 200，实际 %d", probe.Code)
	}
	var st State
	if err := json.Unmarshal(probe.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.AuthEnabled || st.AuthSession != "" || !st.AuthRequired {
		t.Errorf("探针状态错误：enabled=%v session=%q required=%v", st.AuthEnabled, st.AuthSession, st.AuthRequired)
	}
	// 探针不得回传已鉴权内容：响应里连 api_key / accounts 字段都不应出现
	body := probe.Body.String()
	if strings.Contains(body, "api_key") || strings.Contains(body, "accounts") || strings.Contains(body, "proxies") {
		t.Errorf("未登录探针不得泄露面板内容：%s", body)
	}

	// 错误密码
	if w := doReq(mux, "POST", "/api/auth/login", `{"password":"wrong"}`, nil); w.Code != 401 {
		t.Fatalf("错误密码应 401，实际 %d", w.Code)
	}

	// 正确密码 → 下发 HttpOnly cookie
	w = doReq(mux, "POST", "/api/auth/login", `{"password":"s3cret-pass"}`, nil)
	if w.Code != 200 {
		t.Fatalf("登录应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("登录未下发 cookie")
	}
	ck := cookies[0]
	if !ck.HttpOnly || ck.Name != sessionCookie || ck.Value == "" {
		t.Fatalf("cookie 异常：%+v", ck)
	}
	var lr struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &lr)
	if lr.Session == "" || !strings.Contains(w.Body.String(), "session") {
		t.Errorf("登录响应应回传口令指纹，实际 %s", w.Body.String())
	}

	// 带 cookie 访问受保护端点
	if w := doReq(mux, "GET", "/api/logs", "", ck); w.Code != 200 {
		t.Fatalf("带会话访问应 200，实际 %d", w.Code)
	}
	// 探针此时应回显指纹
	probe = doReq(mux, "GET", "/api/auth/state", "", ck)
	if err := json.Unmarshal(probe.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.AuthSession != lr.Session {
		t.Errorf("探针 session=%q 应等于登录指纹 %q", st.AuthSession, lr.Session)
	}
	if st.APIKey == "" {
		t.Error("已登录探针应等价于 /api/state（含 api_key）")
	}

	// 伪造 cookie 无效
	if w := doReq(mux, "GET", "/api/logs", "", &http.Cookie{Name: sessionCookie, Value: "deadbeef"}); w.Code != 401 {
		t.Fatalf("伪造 cookie 应 401，实际 %d", w.Code)
	}

	// 登出后旧 cookie 失效
	if w := doReq(mux, "POST", "/api/auth/logout", "", ck); w.Code != 200 {
		t.Fatalf("登出应 200，实际 %d", w.Code)
	}
	if w := doReq(mux, "GET", "/api/logs", "", ck); w.Code != 401 {
		t.Fatalf("登出后应 401，实际 %d", w.Code)
	}
}

// TestSetListenRequiresPassword 非环回监听必须已有密码；环回监听不受限。
func TestSetListenRequiresPassword(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	if err := a.SetListen("0.0.0.0", 17863); err == nil {
		t.Fatal("无密码时切到 0.0.0.0 应被拒绝")
	}
	if err := a.SetAdminPassword("pw12345678"); err != nil {
		t.Fatalf("设置密码应成功：%v", err)
	}
	if err := a.SetListen("0.0.0.0", 17863); err != nil {
		t.Fatalf("有密码后切到 0.0.0.0 应成功：%v", err)
	}
	// 处于对外监听时不允许清空密码
	if err := a.SetAdminPassword(""); err == nil {
		t.Fatal("对外监听时清空密码应被拒绝")
	}
	if err := a.SetListen("127.0.0.1", 17864); err != nil {
		t.Fatalf("切回环回应成功：%v", err)
	}
	if err := a.SetAdminPassword(""); err != nil {
		t.Fatalf("环回监听下清空密码应成功：%v", err)
	}
}

// TestAdminPasswordChangeRevokesSessions 改密码后旧会话立即失效。
func TestAdminPasswordChangeRevokesSessions(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "first-pass")
	mux := panelMux(a)
	w := doReq(mux, "POST", "/api/auth/login", `{"password":"first-pass"}`, nil)
	ck := w.Result().Cookies()[0]
	if w := doReq(mux, "GET", "/api/logs", "", ck); w.Code != 200 {
		t.Fatalf("登录后应可访问，实际 %d", w.Code)
	}
	if err := a.SetAdminPassword("second-pass"); err != nil {
		t.Fatal(err)
	}
	if w := doReq(mux, "GET", "/api/logs", "", ck); w.Code != 401 {
		t.Fatalf("改密码后旧会话应 401，实际 %d", w.Code)
	}
	// 新密码可登录
	if w := doReq(mux, "POST", "/api/auth/login", `{"password":"second-pass"}`, nil); w.Code != 200 {
		t.Fatalf("新密码登录应 200，实际 %d", w.Code)
	}
}

// TestLoginRateLimit 连续失败达上限后，即使密码正确也被临时拒绝。
func TestLoginRateLimit(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "right-pass")
	mux := panelMux(a)
	for i := 0; i < loginMaxFail; i++ {
		doReq(mux, "POST", "/api/auth/login", `{"password":"bad"}`, nil)
	}
	w := doReq(mux, "POST", "/api/auth/login", `{"password":"right-pass"}`, nil)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "尝试次数过多") {
		t.Fatalf("应限流拒绝，实际 %d %s", w.Code, w.Body.String())
	}
}

// TestListenIsLoopback Loopback 判定的边界。
func TestListenIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true, "localhost": true, "::1": true, "127.0.0.5": true,
		"0.0.0.0": false, "192.168.1.10": false, "": false, "::": false,
	}
	for host, want := range cases {
		if got := (config.Listen{Host: host}).IsLoopback(); got != want {
			t.Errorf("IsLoopback(%q)=%v want %v", host, got, want)
		}
	}
}

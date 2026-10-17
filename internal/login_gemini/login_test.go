// login_test.go 假上游回归测试：**全程本机 httptest / 本机 loopback，不发任何真实网络请求**
// （AGENTS.md §0.1：验证上游行为一律最小化请求数，本文件一次都不打真实上游）。
//
// 覆盖协议 §2.3 里每一条硬约束：
//   - 授权 URL 形态（loopback 回调 + 6 项 scope + state，**无 PKCE**、access_type=offline）；
//   - **先监听再拼 URL**（redirect_uri 与 state 文件一致）、动态端口 + 双栈监听 + 兜底；
//   - 回调 **state 强制校验**（不匹配回 400 且 Poll 报终态错误）；
//   - 换码表单**必须带 client_secret**（只发 client_id 会 invalid_request）；
//   - 身份优先取 `id_token`，缺席才兜底 userinfo；
//   - SaveAuth 落盘 `gemini-<uid>.json` 且 glob 边界不吞 `gemini2-*.json`。
package login_gemini

import (
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/gemini"
)

// fakeIDToken 造一个只含 sub/email 的"id_token"（不验签，解码 payload 即得身份）。
func fakeIDToken(sub, email string) string {
	payload := fmt.Sprintf(`{"sub":%q,"email":%q}`, sub, email)
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// withEndpoints 把登录包用到的端点指向假上游，并在用例结束后还原。
func withEndpoints(t *testing.T, token, userInfo string) {
	t.Helper()
	oldAuth, oldToken, oldUser := authEndpoint, tokenEndpoint, userInfoEndpoint
	if token != "" {
		tokenEndpoint = token
	}
	if userInfo != "" {
		userInfoEndpoint = userInfo
	}
	t.Cleanup(func() { authEndpoint, tokenEndpoint, userInfoEndpoint = oldAuth, oldToken, oldUser })
}

// startLogin 起一次登录并返回 (authURL, statePath, state 文件内容)。
func startLogin(t *testing.T) (string, string, state) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	authURL, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("state 未落盘：%v", err)
	}
	t.Cleanup(Shutdown)
	return authURL, statePath, st
}

// TestStartAuthURLShape 授权 URL 的参数形态逐条对照协议 §2.3（含**无 PKCE**）。
func TestStartAuthURLShape(t *testing.T) {
	authURL, _, st := startLogin(t)

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("授权 URL 不可解析：%v", err)
	}
	if u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
		t.Errorf("授权端点错误：%s", authURL)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":              gemini.ClientID(),
		"response_type":          "code",
		"redirect_uri":           st.RedirectURI,
		"scope":                  strings.Join(gemini.Scopes, " "),
		"state":                  st.State,
		"access_type":            "offline",
		"include_granted_scopes": "true",
		"prompt":                 "consent",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("授权参数 %s = %q，期望 %q", k, q.Get(k), v)
		}
	}
	// ⚠️ 无 PKCE（协议 §2.3 明确「刻意去掉」）。
	for _, banned := range []string{"code_challenge", "code_challenge_method", "code_verifier"} {
		if q.Get(banned) != "" {
			t.Errorf("本渠道**无 PKCE**，不得出现 %s", banned)
		}
	}
	// 回调地址形态：http://localhost:<动态端口>/oauth-callback（**必须是 localhost**，
	// 不能是 127.0.0.1 —— 那是 client 注册值）。
	if !strings.HasPrefix(st.RedirectURI, "http://localhost:") ||
		!strings.HasSuffix(st.RedirectURI, gemini.CallbackPath) {
		t.Errorf("回调地址形态错误：%s", st.RedirectURI)
	}
	// state = randomBytes(16).hex → 32 个小写 hex 字符。
	if len(st.State) != 32 || strings.Trim(st.State, "0123456789abcdef") != "" {
		t.Errorf("state 必须是 32 位 hex：%q", st.State)
	}
	if st.CallbackPort <= 0 || !strings.Contains(st.RedirectURI, fmt.Sprintf(":%d", st.CallbackPort)) {
		t.Errorf("state 里的端口应与 redirect_uri 一致：%+v", st)
	}
}

// TestPollPendingBeforeCallback 没回调时 Poll 返回 ErrPending（不是错误）。
func TestPollPendingBeforeCallback(t *testing.T) {
	_, statePath, _ := startLogin(t)
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("期望 ErrPending，得 %v", err)
	}
}

// TestCallbackRejectsWrongState state 不匹配必须 400 拒绝，且 Poll 报终态错误（不写 code）。
func TestCallbackRejectsWrongState(t *testing.T) {
	_, statePath, st := startLogin(t)
	base := fmt.Sprintf("http://127.0.0.1:%d%s", st.CallbackPort, gemini.CallbackPath)

	resp, err := http.Get(base + "?code=SECRET_CODE&state=deadbeef")
	if err != nil {
		t.Fatalf("回调请求失败：%v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("state 不匹配必须回 400，得 %d（body=%s）", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "SECRET_CODE") {
		t.Error("拒绝页不得回显授权码")
	}

	_, err = Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("Poll 应报 state 校验失败，得 %v", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("终态失败后应清掉 state 文件：%v", statErr)
	}
}

// TestExchangeSendsClientSecretAndCompletes 快乐路径：换码**必须**带 client_secret，
// 身份优先取 id_token（此时**不打** userinfo 兜底）。
func TestExchangeSendsClientSecretAndCompletes(t *testing.T) {
	var (
		gotForm url.Values
		hits    int
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		raw, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(raw))
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			t.Errorf("换码必须是 form-urlencoded，得 %q", ct)
		}
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"access_token":"ya29.ACCESS","refresh_token":"1//REFRESH","expires_in":3600,`+
				`"token_type":"Bearer","scope":"openid","id_token":%q}`, fakeIDToken("sub-123", "a@b.c")))
	}))
	defer tokenSrv.Close()
	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("id_token 已带身份时**不得**再打 userinfo")
	}))
	defer userInfoSrv.Close()
	withEndpoints(t, tokenSrv.URL, userInfoSrv.URL)

	_, statePath, st := startLogin(t)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=CODE-1&state=%s",
		st.CallbackPort, gemini.CallbackPath, st.State))
	if err != nil {
		t.Fatalf("回调请求失败：%v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("合法回调应回 200，得 %d（%s）", resp.StatusCode, page)
	}
	if !strings.Contains(string(page), "已收到授权") {
		t.Errorf("回调页文案应为「已收到授权」（此刻尚未兑换）：%s", page)
	}

	r, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if hits != 1 {
		t.Errorf("换码应恰好一次，得 %d", hits)
	}
	// ⚠️ token 端点强制校验客户端身份：只发 client_id 会 invalid_request。
	if gotForm.Get("client_secret") != gemini.ClientSecret() || gotForm.Get("client_secret") == "" {
		t.Errorf("换码表单**必须**带 client_secret，得 %v", gotForm)
	}
	if gotForm.Get("client_id") != gemini.ClientID() || gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("换码表单错误：%v", gotForm)
	}
	if gotForm.Get("code") != "CODE-1" || gotForm.Get("redirect_uri") != st.RedirectURI {
		t.Errorf("换码表单的 code / redirect_uri 必须与授权请求逐字一致：%v", gotForm)
	}
	if r.AccessToken != "ya29.ACCESS" || r.RefreshToken != "1//REFRESH" {
		t.Errorf("令牌解析错误：%+v", r)
	}
	if r.UID != "sub-123" || r.Nickname != "a@b.c" {
		t.Errorf("身份应取自 id_token（sub/email）：%+v", r)
	}
	if r.ExpiresIn != 3600 || r.ExpiresAt == 0 {
		t.Errorf("到期时刻应由 expires_in（**秒**）写入：%+v", r)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("成功后应清掉 state 文件：%v", statErr)
	}
	// 重复 Poll：state 文件已删 → 报读不到 state，而不是又兑一次码。
	if _, err := Poll(NewClient(), statePath); err == nil {
		t.Error("state 已删除后再次 Poll 必须报错")
	}
}

// TestExchangeTerminalOnRejectedCode 兑换被拒（4xx）属终态：清 state、报错文案只带信封字段。
func TestExchangeTerminalOnRejectedCode(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Bad Request"}`)
	}))
	defer tokenSrv.Close()
	withEndpoints(t, tokenSrv.URL, "")

	_, statePath, st := startLogin(t)
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=C&state=%s",
		st.CallbackPort, gemini.CallbackPath, st.State)); err != nil {
		t.Fatalf("回调请求失败：%v", err)
	}
	_, err := Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("应报 OAuth 信封错误，得 %v", err)
	}
	if strings.Contains(err.Error(), "ya29.") {
		t.Errorf("错误文案不得回显凭据：%v", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Errorf("终态失败应清掉 state 文件：%v", statErr)
	}
}

// TestUserInfoFallbackOnlyWhenIDTokenLacksIdentity 只有 id_token 缺身份时才兜底 userinfo。
func TestUserInfoFallbackOnlyWhenIDTokenLacksIdentity(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// id_token 只带 email（没有 sub）⇒ sub 需走 userinfo 兜底。
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"access_token":"ya29.A","refresh_token":"1//R","expires_in":3600,`+
				`"id_token":%q}`, fakeIDToken("", "only@email.com")))
	}))
	defer tokenSrv.Close()
	var userInfoHits int
	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userInfoHits++
		if got := r.Header.Get("Authorization"); got != "Bearer ya29.A" {
			t.Errorf("userinfo 必须带 Bearer：%q", got)
		}
		_, _ = io.WriteString(w, `{"id":"sub-from-userinfo","email":"only@email.com"}`)
	}))
	defer userInfoSrv.Close()
	withEndpoints(t, tokenSrv.URL, userInfoSrv.URL)

	_, statePath, st := startLogin(t)
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=C&state=%s",
		st.CallbackPort, gemini.CallbackPath, st.State)); err != nil {
		t.Fatalf("回调请求失败：%v", err)
	}
	r, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if userInfoHits != 1 {
		t.Errorf("应恰好兜底一次 userinfo，得 %d", userInfoHits)
	}
	if r.UID != "sub-from-userinfo" || r.Nickname != "only@email.com" {
		t.Errorf("兜底身份解析错误：%+v", r)
	}
}

// TestCallbackPathAndUnknownPath 只处理注册路径；其它路径 404（不写 pending）。
func TestCallbackPathAndUnknownPath(t *testing.T) {
	_, _, st := startLogin(t)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/other", st.CallbackPort))
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("未注册路径应 404，得 %d", resp.StatusCode)
	}
}

// TestListenCallbackDynamicAndFallback 端口选择：默认动态端口；指定端口被占用时换空闲端口；
// 至少绑住 127.0.0.1（[::1] 视机器 IPv6 可用性可容忍失败）。
func TestListenCallbackDynamicAndFallback(t *testing.T) {
	lns, port, err := listenCallback(0)
	if err != nil {
		t.Fatalf("动态端口监听失败：%v", err)
	}
	if port <= 0 || len(lns) == 0 {
		t.Fatalf("动态端口应分配成功：port=%d lns=%d", port, len(lns))
	}
	for _, ln := range lns {
		if got := ln.Addr().String(); !strings.HasPrefix(got, "127.0.0.1:") && !strings.HasPrefix(got, "[::1]:") {
			t.Errorf("只允许环回监听，得 %s", got)
		}
	}
	closeAll(lns)

	// 占用一个端口，再要求监听它 ⇒ 必须换到别的端口（**不能**先拼 URL 再发现端口被占）。
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占用端口失败：%v", err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	lns2, port2, err := listenCallback(busyPort)
	if err != nil {
		t.Fatalf("端口被占时应换端口，得错误 %v", err)
	}
	defer closeAll(lns2)
	if port2 == busyPort {
		t.Errorf("端口被占用时不得继续用 %d", busyPort)
	}
	if port2 <= 0 {
		t.Errorf("换端口后必须拿到有效端口：%d", port2)
	}
}

// TestSaveAuthAndGlobBoundary 落盘形态与 auth.Parse / auth.LoadGeminiDir 同源，
// 且 `gemini-*.json` 不吞 `gemini2-*.json`。
func TestSaveAuthAndGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	r := Result{
		AccessToken:   "ya29.ACCESS",
		RefreshToken:  "1//REFRESH",
		ExpiresAt:     1800000000,
		ExpiresIn:     3600,
		UID:           "sub-123",
		Nickname:      "a@b.c",
		Project:       "CCP-TWO",
		PaidTierID:    "g1-pro-tier",
		PaidTierName:  "Google AI Pro",
		PaidTierLabel: "Pro",
	}
	fp, err := SaveAuth(dir, r)
	if err != nil {
		t.Fatalf("SaveAuth 失败：%v", err)
	}
	if filepath.Base(fp) != "gemini-sub-123.json" {
		t.Fatalf("文件名必须是 gemini-<uid>.json，得 %s", filepath.Base(fp))
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读取落盘文件失败：%v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("落盘文件必须能被 auth.Parse 读取（不变式 3）：%v", err)
	}
	if a.AccessToken != "ya29.ACCESS" || a.RefreshToken != "1//REFRESH" || a.ExpiresAt != 1800000000 {
		t.Errorf("凭据字段落盘错误：%+v", a)
	}
	if a.UID != "sub-123" || a.Nickname != "a@b.c" {
		t.Errorf("账号字段落盘错误：%+v", a)
	}
	// 渠道专有字段进 Extra；通用字段**不得**重复塞进 Extra。
	wantExtra := map[string]string{
		gemini.ExtraKeyProject:       "CCP-TWO",
		gemini.ExtraKeyPaidTier:      "g1-pro-tier",
		gemini.ExtraKeyPaidTierName:  "Google AI Pro",
		gemini.ExtraKeyPaidTierLabel: "Pro",
	}
	for k, v := range wantExtra {
		if a.Extra[k] != v {
			t.Errorf("Extra[%s]=%q，期望 %q", k, a.Extra[k], v)
		}
	}
	for _, banned := range []string{"access_token", "refresh_token", "uid", "nickname", "expires_at"} {
		if _, has := a.Extra[banned]; has {
			t.Errorf("Extra 不得重复承载 %s", banned)
		}
	}

	loaded, err := auth.LoadGeminiDir(dir)
	if err != nil {
		t.Fatalf("LoadGeminiDir 失败：%v", err)
	}
	if len(loaded) != 1 || loaded[0].Kind != gemini.Kind.String() || loaded[0].UID != "sub-123" {
		t.Fatalf("LoadGeminiDir 结果错误：%+v", loaded)
	}

	// glob 边界：`gemini2-*.json` 不得被 `gemini-*.json` 吞掉。
	decoy := filepath.Join(dir, "gemini2-other.json")
	if err := os.WriteFile(decoy, []byte(`{"auth":{"accessToken":"x"},"account":{"uid":"other"}}`), 0o600); err != nil {
		t.Fatalf("写入干扰文件失败：%v", err)
	}
	loaded, err = auth.LoadGeminiDir(dir)
	if err != nil {
		t.Fatalf("LoadGeminiDir 失败：%v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("gemini-*.json 不得吞 gemini2-*.json，得 %d 条", len(loaded))
	}

	// 半成品凭据必须拒（落盘后不可用且会污染账号池）。
	if _, err := SaveAuth(dir, Result{AccessToken: "x"}); err == nil {
		t.Error("缺 uid 必须拒绝")
	}
	if _, err := SaveAuth(dir, Result{UID: "u"}); err == nil {
		t.Error("缺 access_token 必须拒绝")
	}
}

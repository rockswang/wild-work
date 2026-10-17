package login_loomy

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
)

// fakeUpstream 是一个**假的讯飞账号服务**（httptest）。
//
// 全部测试只用本机假上游：AGENTS.md §0.1 严禁对真实上游做批量/穷举探测
// （2026-10-16 曾因批量探测被封号）。故本文件没有任何指向生产域名的请求。
type fakeUpstream struct {
	mu sync.Mutex

	// wantCode 是 checkCode 认为正确的验证码。
	wantCode string
	// msgid 由 sendMsgCode 下发，checkCode 必须原样带回。
	msgid string
	// session / userid 是登录成功时下发的凭据（假值，绝不是真 token）。
	session string
	userid  string

	sentCount   int
	checkCount  int
	lastHeaders http.Header
	lastBody    map[string]any
	// rejectMsg 非空时，checkCode 直接回该错误（模拟验证码错误）。
	rejectMsg string
	// missingUserID 为真时，checkCode 下发 session 但**不给** userid。
	missingUserID bool
}

const (
	testMsgID   = "msgid-abc-123"
	testSession = "0123456789abcdef0123456789abcdef" // 32 位 hex（与真实 session 同形，但是假的）
	testUserID  = "u_10086"
)

func (f *fakeUpstream) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login/phone/sendMsgCode", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sentCount++
		f.lastHeaders = r.Header.Clone()
		f.lastBody = decodeEnvelope(t, r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "0", "desc": "success", "sid": "s1",
			"data": map[string]any{"msgid": f.msgid},
		})
	})
	mux.HandleFunc("/login/phone/checkCode", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.checkCount++
		f.lastHeaders = r.Header.Clone()
		f.lastBody = decodeEnvelope(t, r.Body)

		param, _ := f.lastBody["param"].(map[string]any)
		if f.rejectMsg != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "020002", "desc": f.rejectMsg})
			return
		}
		if got, _ := param["mcode"].(string); got != f.wantCode {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "020001", "desc": "验证码错误"})
			return
		}
		if got, _ := param["msgid"].(string); got != f.msgid {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "020003", "desc": "msgid 无效"})
			return
		}
		data := map[string]any{"session": f.session}
		if !f.missingUserID {
			data["userid"] = f.userid
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "0", "desc": "success", "data": data})
	})
	return mux
}

// decodeEnvelope 解 {base, param} 信封。
func decodeEnvelope(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读请求体失败：%v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("请求体不是 JSON：%v（原文 %q）", err, string(raw))
	}
	return m
}

// newFake 起一个假上游并让它成为默认端点；测试结束自动关闭并复位全局。
func newFake(t *testing.T, mutate func(*fakeUpstream)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		wantCode: "123456",
		msgid:    testMsgID,
		session:  testSession,
		userid:   testUserID,
	}
	if mutate != nil {
		mutate(f)
	}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)

	old := accountBase
	accountBase = srv.URL
	t.Cleanup(func() { accountBase = old })
	return f
}

// isolate 隔离包级会话，避免测试相互污染。
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	Shutdown()
	t.Cleanup(Shutdown)
	return statePath
}

// ===========================================================================
// 授权入口 URL 与端点常量
// ===========================================================================

// TestLoginURLIsNotLoopback LoginURL 常量是「逻辑入口」文案锚点，不得指向本机。
//
// 真正给前端打开的是 Start 返回的本机引导页地址；本常量只用于说明文案与测试锚点，
// 故它必须是上游 https 地址（防止有人误把它当成可用的登录入口）。
func TestLoginURLIsNotLoopback(t *testing.T) {
	if !strings.HasPrefix(LoginURL, "https://") {
		t.Fatalf("LoginURL 必须是 https：%s", LoginURL)
	}
	if strings.Contains(LoginURL, "127.0.0.1") || strings.Contains(LoginURL, "localhost") {
		t.Fatalf("LoginURL 不应指向本机：%s", LoginURL)
	}
	if !strings.Contains(LoginURL, "loomy") {
		t.Fatalf("LoginURL 应指向 Loomy 站点：%s", LoginURL)
	}
}

// TestEndpointConstants 端点常量必须与 ofm 的实现一致（唯一来源，防止漂移）。
func TestEndpointConstants(t *testing.T) {
	if got := accountBase; got != "https://account.xfinfr.com" {
		t.Fatalf("accountBase 不符：%s", got)
	}
	if apiHost != "https://loomyad.xunfei.cn" {
		t.Fatalf("apiHost 不符：%s", apiHost)
	}
	if apiDomain != "/api/v1" {
		t.Fatalf("apiDomain 不符：%s", apiDomain)
	}
	if sessionTTLSeconds != 1209600 {
		t.Fatalf("sessionTTLSeconds 应为 14 天（1209600），实际 %d", sessionTTLSeconds)
	}
	if smsCodeTTLSeconds != 300 {
		t.Fatalf("smsCodeTTLSeconds 应为 300，实际 %d", smsCodeTTLSeconds)
	}
	if epSendCode != "/login/phone/sendMsgCode" || epCheckCode != "/login/phone/checkCode" {
		t.Fatalf("端点路径不符：%q %q", epSendCode, epCheckCode)
	}
}

// ===========================================================================
// HMAC-SHA1 签名（ofm loomy-sign.ts 的逐段对应）
// ===========================================================================

// TestSignMatchesReferenceVector 用**手算的**签名串校验拼串格式。
//
// 签名串是 9 段 \n 连接，后两段（SignedHeaders / CanonicalizedHeaders）恒为空，
// 故字符串以 \n\n 结尾 —— 去掉尾随换行会让签名不匹配（ofm loomy-sign.ts:14-17）。
func TestSignMatchesReferenceVector(t *testing.T) {
	body := `{"base":{"appid":"GM3LOOMY"},"param":{}}`
	md5 := contentMD5(body)
	if md5 == "" {
		t.Fatal("非空 body 的 Content-MD5 不能为空")
	}
	if contentMD5("") != "" {
		t.Fatal("空 body 的 Content-MD5 必须是空串（与 sign.js 一致）")
	}

	got := buildSigningString("POST", "/login/phone/sendMsgCode", "", md5,
		"application/json", "Tue, 17 Oct 2026 00:00:00 GMT", "nonce-1")
	want := strings.Join([]string{
		"POST", "/login/phone/sendMsgCode", "", md5, "application/json",
		"Tue, 17 Oct 2026 00:00:00 GMT", "nonce-1", "", "",
	}, "\n")
	if got != want {
		t.Fatalf("签名字符串不符\n期望 %q\n实际 %q", want, got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatal("签名串必须以两个换行结尾（后两段恒为空）")
	}
}

// TestEscapedPath 路径转义：补前导 /、剥末尾 /、逐段 RFC3986。
func TestEscapedPath(t *testing.T) {
	cases := map[string]string{
		"/login/phone/sendMsgCode": "/login/phone/sendMsgCode",
		"login/phone/sendMsgCode":  "/login/phone/sendMsgCode",
		"/login/phone/":            "/login/phone",
		"/":                        "/",
		"/a b/c":                   "/a%20b/c",
		"/a*/b":                    "/a%2A/b",
		"/a!b/c":                   "/a%21b/c",
	}
	for in, want := range cases {
		if got := escapedPath(in); got != want {
			t.Errorf("escapedPath(%q) 期望 %q 实际 %q", in, want, got)
		}
	}
}

// TestAuthHeaders 认证头形态：Authorization: account {ak}:{sig}（不是 Bearer）。
func TestAuthHeaders(t *testing.T) {
	body := `{"base":{},"param":{}}`
	const date = "Tue, 17 Oct 2026 00:00:00 GMT"
	const nonce = "nonce-1"
	h := authHeaders("POST", "/login/phone/checkCode", "", body, "application/json", date, nonce)

	if !strings.HasPrefix(h["Authorization"], "account ") {
		t.Fatalf("Authorization 前缀应为 account：%q", h["Authorization"])
	}
	if strings.Contains(h["Authorization"], "Bearer") {
		t.Fatalf("认证头不得是 Bearer：%q", h["Authorization"])
	}
	if h["Date"] != date || h["Nonce"] != nonce || h["Content-Type"] != "application/json" {
		t.Fatalf("必需头不符：%+v", h)
	}
	// 独立复算签名，确认签名串与密钥用法一致。
	mac := hmac.New(sha1.New, []byte(accessKeySecret))
	mac.Write([]byte(buildSigningString("POST", "/login/phone/checkCode", "",
		contentMD5(body), "application/json", date, nonce)))
	want := "account " + accessKeyID + ":" + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if h["Authorization"] != want {
		t.Fatalf("签名不符\n期望 %q\n实际 %q", want, h["Authorization"])
	}
}

// ===========================================================================
// SendCode / Login（httptest）
// ===========================================================================

// TestSendCodeHitsFakeUpstream 发验证码：打到正确路径、信封字段齐全、取回 msgid。
func TestSendCodeHitsFakeUpstream(t *testing.T) {
	f := newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	msgid, err := SendCode(NewClient(), "13800138000")
	if err != nil {
		t.Fatalf("SendCode 失败：%v", err)
	}
	if msgid != testMsgID {
		t.Fatalf("msgid 期望 %q 实际 %q", testMsgID, msgid)
	}
	if f.sentCount != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d", f.sentCount)
	}

	base, _ := f.lastBody["base"].(map[string]any)
	if base == nil {
		t.Fatalf("请求体缺 base 段：%+v", f.lastBody)
	}
	for _, k := range []string{"appid", "modelid", "version", "devid", "ua", "traceid"} {
		if s, _ := base[k].(string); s == "" {
			t.Errorf("base.%s 不能为空（信封必须完整）：%+v", k, base)
		}
	}
	if base["appid"] != accessKeyAppID {
		t.Errorf("base.appid 期望 %q 实际 %v", accessKeyAppID, base["appid"])
	}
	if base["ua"] != "Loomy|Desktop|Electron|macOS" {
		t.Errorf("base.ua 必须硬编码为官方值（Windows 上也是它）：%v", base["ua"])
	}
	param, _ := f.lastBody["param"].(map[string]any)
	if param["phone"] != "13800138000" {
		t.Errorf("param.phone 不符：%v", param["phone"])
	}
	if param["ccode"] != "86" {
		t.Errorf("param.ccode 应为 86：%v", param["ccode"])
	}
	if param["expire"] != float64(300) {
		t.Errorf("param.expire（验证码有效期）应为 300：%v", param["expire"])
	}
	if f.lastHeaders.Get("Content-MD5") == "" {
		t.Error("非空 body 必须带 Content-MD5 头")
	}
	if !strings.HasPrefix(f.lastHeaders.Get("Authorization"), "account ") {
		t.Errorf("Authorization 前缀应为 account：%q", f.lastHeaders.Get("Authorization"))
	}
}

// TestSendCodeRejectsBadPhone 非法手机号在**本地**就拒绝（不发请求、不浪费上游配额）。
func TestSendCodeRejectsBadPhone(t *testing.T) {
	f := newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	for _, bad := range []string{"", "138", "1380013800a", "123456789012", "+8613800138000", "12800138000"} {
		if _, err := SendCode(NewClient(), bad); err == nil {
			t.Errorf("手机号 %q 应被拒", bad)
		}
	}
	if f.sentCount != 0 {
		t.Fatalf("非法手机号不得发请求，实际发了 %d 次", f.sentCount)
	}
}

// TestSendCodeMissingMsgIDIsError 响应缺 msgid 必须报错（不能把空串交给下一步）。
func TestSendCodeMissingMsgIDIsError(t *testing.T) {
	newFake(t, func(f *fakeUpstream) { f.msgid = "" })
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if _, err := SendCode(NewClient(), "13800138000"); err == nil {
		t.Fatal("缺 msgid 时应报错")
	}
	// 下发验证码失败是终态：Poll 必须给出可行动的错误。
	if _, err := Poll(NewClient(), statePath); err == nil || err == ErrPending {
		t.Fatalf("缺 msgid 后 Poll 应为终态错误，实际 %v", err)
	}
}

// TestLoginSuccess 正确验证码 → 拿到 session + userid，且 expire 声明 14 天。
func TestLoginSuccess(t *testing.T) {
	f := newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}

	r, err := Login(NewClient(), "13800138000", "123456", testMsgID)
	if err != nil {
		t.Fatalf("Login 失败：%v", err)
	}
	if r.AccessToken != testSession {
		t.Fatalf("AccessToken 期望 %q 实际 %q", testSession, r.AccessToken)
	}
	if r.UID != testUserID {
		t.Fatalf("UID 应取上游 userid，期望 %q 实际 %q", testUserID, r.UID)
	}
	if r.ExpiresAt <= 0 {
		t.Fatal("ExpiresAt 必须为正（否则 NeedsRefresh 恒真 → 反复刷新必失败）")
	}
	if r.RefreshToken != "" {
		t.Fatal("Loomy 无 refresh 端点，RefreshToken 必须留空")
	}
	if r.Domain != apiDomain || r.ApiHost != apiHost {
		t.Fatalf("Domain/ApiHost 不符：%q %q", r.Domain, r.ApiHost)
	}

	param, _ := f.lastBody["param"].(map[string]any)
	if param["expire"] != float64(1209600) {
		t.Errorf("登录请求的 param.expire 必须是 14 天（1209600）：%v", param["expire"])
	}
	if param["mcode"] != "123456" || param["msgid"] != testMsgID {
		t.Errorf("param 字段不符：%+v", param)
	}
}

// TestLoginWrongCode 验证码错误：透出上游 desc，且不返回半成品凭据、不置终态（可重试）。
func TestLoginWrongCode(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	r, err := Login(NewClient(), "13800138000", "000000", testMsgID)
	if err == nil {
		t.Fatalf("错误验证码应失败，实际拿到 %+v", r)
	}
	if !strings.Contains(err.Error(), "验证码错误") {
		t.Fatalf("错误应透出上游 desc（便于用户判断是输错还是过期），实际：%v", err)
	}
	// 输错验证码**不是**终态：用户应该能就地再试一次。
	if _, perr := Poll(NewClient(), statePath); perr != ErrPending {
		t.Fatalf("输错验证码后流程应仍为 pending（可重试），实际 %v", perr)
	}
	if _, err := Login(NewClient(), "13800138000", "123456", testMsgID); err != nil {
		t.Fatalf("重试应成功：%v", err)
	}
}

// TestLoginMissingUserID 上游不给 userid 时仍有稳定 UID（回落派生），且两次登录同值。
func TestLoginMissingUserID(t *testing.T) {
	newFake(t, func(f *fakeUpstream) { f.missingUserID = true })
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	r1, err := Login(NewClient(), "13800138000", "123456", testMsgID)
	if err != nil {
		t.Fatalf("Login 失败：%v", err)
	}
	if r1.UID == "" {
		t.Fatal("缺 userid 时 UID 必须由回落规则派生，不能为空")
	}
	// 同一份 session 必须得到同一个 UID（否则重复登录会写成两个账号文件）。
	r2, err := Login(NewClient(), "13800138000", "123456", testMsgID)
	if err != nil {
		t.Fatalf("第二次 Login 失败：%v", err)
	}
	if r1.UID != r2.UID {
		t.Fatalf("同一 session 的 UID 应稳定：%q vs %q", r1.UID, r2.UID)
	}
	// 派生 UID 不得是完整 session（完整串等于 token）。
	if r1.UID == r1.AccessToken {
		t.Fatal("派生 UID 不得等于完整 session")
	}
}

// TestLoginMissingSessionIsError 缺 session 必须报错（空 session 落盘 = 死账号）。
func TestLoginMissingSessionIsError(t *testing.T) {
	newFake(t, func(f *fakeUpstream) { f.session = "" })
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if _, err := Login(NewClient(), "13800138000", "123456", testMsgID); err == nil {
		t.Fatal("缺 session 时应报错")
	}
}

// TestLoginRequiresMsgID 没先发验证码（msgid 空）时本地拒绝。
func TestLoginRequiresMsgID(t *testing.T) {
	f := newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if _, err := Login(NewClient(), "13800138000", "123456", ""); err == nil {
		t.Fatal("msgid 为空时应本地报错")
	}
	if f.checkCount != 0 {
		t.Fatalf("msgid 为空不得发请求，实际 %d 次", f.checkCount)
	}
}

// TestLoginErrorsCarryNoToken 错误文案**绝不**包含 session（零 token 落盘，
// 错误会经 app.pollLogin 进日志）。后端若把 token 回显在 desc 里也必须被吃掉。
func TestLoginErrorsCarryNoToken(t *testing.T) {
	newFake(t, func(f *fakeUpstream) { f.rejectMsg = "bad token " + testSession })
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	_, err := Login(NewClient(), "13800138000", "123456", testMsgID)
	if err == nil {
		t.Fatal("应失败")
	}
	if strings.Contains(err.Error(), testSession) {
		t.Fatalf("错误文案泄露了 session：%v", err)
	}

	// 成功路径也不能把 session 放进日志关心的字段。
	newFake(t, nil)
	if _, lerr := Login(NewClient(), "13800138000", "123456", testMsgID); lerr != nil {
		t.Fatalf("Login 失败：%v", lerr)
	}
	r, _ := Login(NewClient(), "13800138000", "123456", testMsgID)
	if strings.Contains(r.LogFields(), r.AccessToken) {
		t.Fatal("LogFields 不得包含 token（日志零 token 红线）")
	}
}

// ===========================================================================
// SaveAuth 落盘
// ===========================================================================

// TestSaveAuthWritesParseableFile 落盘必须是 loomy-<uid>.json + 嵌套 {auth,account}，
// 且能被 internal/auth.Parse 与 auth.LoadLoomyDir 读回（不变式 3 + glob 契约）。
func TestSaveAuthWritesParseableFile(t *testing.T) {
	dir := t.TempDir()
	r := Result{
		AccessToken: testSession,
		ExpiresAt:   1_900_000_000,
		Domain:      apiDomain,
		ApiHost:     apiHost,
		UID:         testUserID,
		Phone:       "13800138000",
		Nickname:    "Loomy 用户",
	}
	fp, err := SaveAuth(dir, r)
	if err != nil {
		t.Fatalf("SaveAuth 失败：%v", err)
	}
	if got := filepath.Base(fp); got != "loomy-"+testUserID+".json" {
		t.Fatalf("文件名不符（LoadLoomyDir 的 glob 是 loomy-*.json）：%s", got)
	}

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var nested struct {
		Auth    map[string]any `json:"auth"`
		Account map[string]any `json:"account"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatalf("落盘不是合法 JSON：%v", err)
	}
	if nested.Auth == nil || nested.Account == nil {
		t.Fatalf("落盘必须是嵌套 auth/account：%s", string(raw))
	}
	if nested.Auth["accessToken"] != testSession {
		t.Fatal("auth.accessToken 不符")
	}
	if nested.Account["uid"] != testUserID {
		t.Fatal("account.uid 不符")
	}

	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("auth.Parse 读回失败：%v", err)
	}
	if a.AccessToken != testSession || a.UID != testUserID || a.ExpiresAt != r.ExpiresAt {
		t.Fatalf("Parse 结果不符：%+v", a)
	}

	got, err := auth.LoadLoomyDir(dir)
	if err != nil {
		t.Fatalf("LoadLoomyDir 失败：%v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLoomyDir 应扫到 1 个账号，实际 %d", len(got))
	}
	if got[0].AccessToken != testSession {
		t.Fatal("LoadLoomyDir 读到的 accessToken 不符")
	}
	if got[0].Kind != "loomy" {
		t.Fatalf("Kind 应为 loomy，实际 %q", got[0].Kind)
	}
}

// TestSaveAuthFileMode 凭据文件必须是 0600（同目录其它渠道口径）。
func TestSaveAuthFileMode(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows 不强制 POSIX 权限位，跳过")
	}
	dir := t.TempDir()
	fp, err := SaveAuth(dir, Result{AccessToken: testSession, ExpiresAt: 1, UID: "u1"})
	if err != nil {
		t.Fatalf("SaveAuth 失败：%v", err)
	}
	st, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("凭据文件权限应为 0600，实际 %o", perm)
	}
}

// TestSaveAuthRejectsEmpty 半成品凭据不得落盘（会污染账号池）。
func TestSaveAuthRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveAuth(dir, Result{UID: "u1"}); err == nil {
		t.Fatal("缺 accessToken 时应拒")
	}
	if _, err := SaveAuth(dir, Result{AccessToken: testSession}); err == nil {
		t.Fatal("缺 uid 时应拒")
	}
}

// TestSaveAuthSanitizesUID UID 里的路径分隔符不得逃出 auths 目录。
func TestSaveAuthSanitizesUID(t *testing.T) {
	dir := t.TempDir()
	fp, err := SaveAuth(dir, Result{AccessToken: testSession, ExpiresAt: 1, UID: "../../evil"})
	if err != nil {
		t.Fatalf("SaveAuth 失败：%v", err)
	}
	if filepath.Dir(fp) != filepath.Clean(dir) {
		t.Fatalf("UID 逃出了 auths 目录：%s", fp)
	}
	if strings.Contains(filepath.Base(fp), "..") {
		t.Fatalf("文件名仍含 ..：%s", fp)
	}
}

// TestSaveAuthOverwritesSameUID 同一账号重登覆盖同一文件（不产生重复账号）。
func TestSaveAuthOverwritesSameUID(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveAuth(dir, Result{AccessToken: "old", ExpiresAt: 1, UID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveAuth(dir, Result{AccessToken: "new", ExpiresAt: 2, UID: "u1"}); err != nil {
		t.Fatal(err)
	}
	got, err := auth.LoadLoomyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("重登应覆盖同一文件，实际 %d 个账号", len(got))
	}
	if got[0].AccessToken != "new" {
		t.Fatal("覆盖后应是新的 accessToken")
	}
}

// TestSaveAuthDoesNotCollideWithOtherChannels 落盘前缀不得被别的渠道 glob 吞掉。
//
// auth.go 的 glob 边界：LoadLobsterAIDir 用 lobsterai-*.json，与本渠道的 loomy-*.json
// 都以 lo 开头但字面量不同（bsterai vs omy），故互不吞。本测试锁死这一点。
func TestSaveAuthDoesNotCollideWithOtherChannels(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveAuth(dir, Result{AccessToken: testSession, ExpiresAt: 1, UID: "u1"}); err != nil {
		t.Fatal(err)
	}
	// 造一个别家渠道的文件，确认不会被 LoadLoomyDir 扫进来。
	if err := os.WriteFile(filepath.Join(dir, "lobsterai-other.json"),
		[]byte(`{"auth":{"accessToken":"x"},"account":{"uid":"other"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := auth.LoadLoomyDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLoomyDir 只能扫到自己的 1 个文件，实际 %d", len(got))
	}
	if got[0].UID != "u1" {
		t.Fatalf("扫到了别家渠道的文件：uid=%q", got[0].UID)
	}
}

// ===========================================================================
// 登录编排：Start / Poll / SendCode / Cancel
// ===========================================================================

// TestStartReturnsGuideURL Start 必须立即返回**本机引导页**的地址，且初始状态是 pending。
//
// 为什么是本机页而不是上游官网：短信验证码要由本工具的签名协议真实下发，
// 官网登录页的验证码不会交给本工具（见包注释）。前端对这个 URL 的处理是「原样 window.open」。
func TestStartReturnsGuideURL(t *testing.T) {
	f := newFake(t, nil)
	statePath := isolate(t)

	u, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("Start 应返回本机引导页地址（127.0.0.1），实际 %q", u)
	}
	if !strings.HasSuffix(u, guidePath) {
		t.Fatalf("引导页路径应为 %q，实际 %q", guidePath, u)
	}
	s := CurrentStatus()
	if s.Phase != PhasePending {
		t.Fatalf("初始状态应为 pending，实际 %q", s.Phase)
	}
	if s.HasSession {
		t.Fatal("尚未登录成功，不应有 session")
	}
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("未登录时 Poll 应返回 ErrPending，实际 %v", err)
	}
	if f.sentCount != 0 {
		t.Fatalf("Start 不应触达上游，实际 %d 次", f.sentCount)
	}
}

// TestStartRejectsConcurrent 已有进行中的流程时**幂等返回同一个引导页地址**（而不是报错）。
//
// 为什么不是报错：前端要拿到 auth_url 才会 window.open。若重复点「登录 Loomy」时这里报错，
// 前端只弹一个 toast、不打开任何窗口，用户就**再也看不到**那个仍然活着的引导页（只能点取消重来）。
// 返回同一个 URL 则让用户无论点什么都能继续，且不会起第二个监听端口。
func TestStartRejectsConcurrent(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	first, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("第一次 Start 失败：%v", err)
	}
	second, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("重复 Start 不应报错（应幂等返回同一地址）：%v", err)
	}
	if first != second {
		t.Fatalf("重复 Start 应返回同一引导页地址：%q vs %q", first, second)
	}
	// 确认没有起第二个监听：老地址仍然只对应一个页面。
	if _, err := http.Get(second); err != nil {
		t.Fatalf("幂等返回的地址应可访问：%v", err)
	}
}

// TestStartAfterSuccessYieldsFreshFlow 成功后再次 Start 必须起**新**流程
// （成功后不需要用户点取消：app.pollLogin 收尾时会 Shutdown 掉上一轮）。
func TestStartAfterSuccessYieldsFreshFlow(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if _, err := Login(NewClient(), "13800138000", "123456", testMsgID); err != nil {
		t.Fatalf("Login 失败：%v", err)
	}
	if _, err := Poll(NewClient(), statePath); err != nil {
		t.Fatalf("Poll 应拿到结果：%v", err)
	}
	// 模拟 app.pollLogin 的 defer：登录结束后关掉引导页。
	Shutdown()

	u, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("成功后再次 Start 应起新流程：%v", err)
	}
	if st := CurrentStatus(); st.Phase != PhasePending || st.HasSession {
		t.Fatalf("新一轮应为干净的 pending 状态，实际 %s", st.String())
	}
	if _, err := http.Get(u); err != nil {
		t.Fatalf("新引导页应可访问：%v", err)
	}
}

// TestFullFlow 端到端：Start → SendCode → Login 成功后 Poll 拿到结果 → 落盘 → 只能取一次。
func TestFullFlow(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	authDir := t.TempDir()

	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if _, err := SendCode(NewClient(), "13800138000"); err != nil {
		t.Fatalf("SendCode 失败：%v", err)
	}
	if _, err := Login(NewClient(), "13800138000", "123456", testMsgID); err != nil {
		t.Fatalf("Login 失败：%v", err)
	}

	r, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll 应拿到结果，实际 %v", err)
	}
	if r.AccessToken != testSession {
		t.Fatalf("Poll 结果不符：%+v", r)
	}
	if _, err := SaveAuth(authDir, r); err != nil {
		t.Fatalf("SaveAuth 失败：%v", err)
	}
	if got, _ := auth.LoadLoomyDir(authDir); len(got) != 1 {
		t.Fatalf("落盘后应扫到 1 个账号，实际 %d", len(got))
	}

	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("结果取走后应回到 ErrPending，实际 %v", err)
	}
}

// TestPollAfterCancel 取消后 Poll 返回 ErrPending（**既有渠道约定**，不是终态错误）。
//
// 依据：login_lobsterai / login_gemini / login_codearts 的 Shutdown 都把会话置 nil，
// 其 Poll 随之返回 ErrPending；取消的文案由 app.pollLogin 的 ctx.Done() 分支统一给出
// （「登录已取消」）。本包遵循同一约定，避免用户看到两条重复提示。
func TestPollAfterCancel(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if st := CurrentStatus(); st.Phase != PhasePending {
		t.Fatalf("Start 后应为 pending，实际 %q", st.Phase)
	}
	Shutdown()
	// Shutdown 之后会话已作废：即便之前 pending，也不能再报成功。
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("取消后 Poll 应返回 ErrPending（取消由 app 层 ctx 分支表述），实际 %v", err)
	}
	// 再 Start 必须是干净的新会话（不是复用刚才那个）。
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("取消后重新 Start 应成功：%v", err)
	}
}

// TestShutdownIdempotent Shutdown 幂等（app 的 CancelLogin 与 pollLogin 的 defer 都会调）。
func TestShutdownIdempotent(t *testing.T) {
	isolate(t)
	Shutdown()
	Shutdown()
}

// TestCancelStopsGuideServer 取消后引导页必须真的关掉（端口不泄漏、页面不可再访问）。
func TestCancelStopsGuideServer(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	u, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("引导页应可访问：%v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "登录 Loomy") {
		t.Fatalf("引导页内容不符：%s", truncateForLog(string(body), 200))
	}

	Shutdown()
	if _, err := http.Get(u); err == nil {
		t.Fatal("Shutdown 后引导页必须不可访问（端口应已关闭）")
	}
}

// TestSendCodeNeedsFlow 没有 Start 就发验证码应报错（避免无流程时留悬空状态）。
func TestSendCodeNeedsFlow(t *testing.T) {
	newFake(t, nil)
	isolate(t)
	if _, err := SendCode(NewClient(), "13800138000"); err == nil {
		t.Fatal("未 Start 时 SendCode 应报错")
	}
	if _, err := Login(NewClient(), "13800138000", "123456", testMsgID); err == nil {
		t.Fatal("未 Start 时 Login 应报错")
	}
}

// TestGuidePageAPIFlow 直接打引导页的 HTTP 接口（用户实际的操作路径）。
func TestGuidePageAPIFlow(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	u, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	base := strings.TrimSuffix(u, guidePath)

	post := func(path string, payload map[string]any) map[string]any {
		raw, _ := json.Marshal(payload)
		resp, err := http.Post(base+path, "application/json", strings.NewReader(string(raw)))
		if err != nil {
			t.Fatalf("POST %s 失败：%v", path, err)
		}
		defer resp.Body.Close()
		var m map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			t.Fatalf("POST %s 响应不是 JSON：%v", path, err)
		}
		return m
	}

	// 1) 发验证码
	sent := post(apiSendCodePath, map[string]any{"phone": "13800138000"})
	if sent["ok"] != true {
		t.Fatalf("发验证码失败：%+v", sent)
	}
	msgid, _ := sent["msgid"].(string)
	if msgid != testMsgID {
		t.Fatalf("msgid 不符：%v", sent["msgid"])
	}
	if msg, _ := sent["message"].(string); strings.Contains(msg, "13800138000") {
		t.Fatalf("回执里不该出现完整手机号：%s", msg)
	}

	// 2) 验证码错误 → 就地报错、流程仍 pending
	bad := post(apiLoginPath, map[string]any{"phone": "13800138000", "code": "000000", "msgid": msgid})
	if bad["ok"] == true {
		t.Fatalf("错误验证码不该成功：%+v", bad)
	}
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("输错验证码后仍应 pending，实际 %v", err)
	}

	// 3) 正确验证码 → 成功 → Poll 可收尾
	ok := post(apiLoginPath, map[string]any{"phone": "13800138000", "code": "123456", "msgid": msgid})
	if ok["ok"] != true {
		t.Fatalf("登录失败：%+v", ok)
	}
	r, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll 应拿到结果：%v", err)
	}
	if r.AccessToken != testSession {
		t.Fatalf("结果不符：%+v", r)
	}
}

// TestGuidePageServesHTML 引导页是自包含 HTML（无外部资源，断网可显示）。
func TestGuidePageServesHTML(t *testing.T) {
	newFake(t, nil)
	statePath := isolate(t)
	u, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("访问引导页失败：%v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type 应为 text/html：%s", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{"登录 Loomy", apiSendCodePath, apiLoginPath} {
		if !strings.Contains(html, want) {
			t.Errorf("引导页缺少 %q", want)
		}
	}
	// 零外部资源：不得引用 http(s) 资源（否则断网白屏）。
	for _, bad := range []string{`src="http`, `href="http`} {
		if strings.Contains(html, bad) {
			t.Errorf("引导页不得引用外部资源：%s", bad)
		}
	}
	// 页面里绝不能出现 token 或密钥。
	if strings.Contains(html, testSession) || strings.Contains(html, accessKeySecret) {
		t.Error("引导页泄露了 token / 密钥")
	}
}

// TestMaskedPhoneAndStatusNoToken 日志/状态里手机号脱敏、且状态串不含 token。
func TestMaskedPhoneAndStatusNoToken(t *testing.T) {
	if got := maskPhone("13800138000"); got != "138****8000" {
		t.Fatalf("maskPhone 期望 138****8000 实际 %q", got)
	}
	if got := maskPhone("138"); got != "" {
		t.Fatalf("过短手机号应脱敏成空串，实际 %q", got)
	}

	newFake(t, nil)
	statePath := isolate(t)
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	if _, err := SendCode(NewClient(), "13800138000"); err != nil {
		t.Fatalf("SendCode 失败：%v", err)
	}
	if _, err := Login(NewClient(), "13800138000", "123456", testMsgID); err != nil {
		t.Fatalf("Login 失败：%v", err)
	}
	s := CurrentStatus()
	if !s.HasSession {
		t.Fatal("登录成功后 HasSession 应为 true")
	}
	if strings.Contains(s.String(), testSession) {
		t.Fatalf("Status 泄露了 session：%s", s.String())
	}
	if strings.Contains(s.String(), "13800138000") {
		t.Fatalf("Status 泄露了完整手机号：%s", s.String())
	}
	if s.String() == "" {
		t.Fatal("Status 应有可读描述")
	}
}

// truncateForLog 截断长文本（仅测试断言消息用）。
func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

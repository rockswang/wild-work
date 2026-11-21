package app

// chatgpt_auth_test.go 锁死票 05 的「面板粘贴网页凭据」链路：
//
//	面板 → POST /api/login/chatgpt_auth → App.SubmitChatGPTAuth
//	     → chatgpt.Client.Me（零配额校验，httptest 假上游）→ 落盘 auths/chatgpt-<uid>.json
//	     → reloadAccounts（账号立刻进池）+ afterAccountAdded（模型目录重拉）
//
// 为什么要单独锁（每一条漏改都不报错）：
//   - 校验前落盘 → 用户拿到一个「每次请求都 403」的账号，且没有任何提示；
//   - 只认一段凭据（cookie 或 token）→ 同上，「看起来成功」；
//   - uid 编造/退回随机值 → 同一账号每贴一次就多一个条目，积分/状态看似翻倍；
//   - 不调 reloadAccounts → 账号进不了池，面板要重启才看得到；
//   - 错误串带凭据明文 → 违反零 token 纪律（不变量 2）。
//
// 上游的行为（Me/Session 的解析细节）由 internal/chatgpt 的单测覆盖；这里只验 App 侧接线。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/chatgpt"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// fakeChatGPT 是 /backend-api/me 与 /api/auth/session 的极简假上游。
type fakeChatGPT struct {
	mu       sync.Mutex
	meCalls  int
	sessCall int
	// meStatus 非 200 时按该状态 + meBody 回错（测失败分支）。
	meStatus int
	meBody   string
	// wantCookie 非空时要求请求 Cookie 头包含它（证明 Cookie 真的发出去了）。
	wantCookie string
}

func (f *fakeChatGPT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case chatgpt.PathMe:
		f.mu.Lock()
		f.meCalls++
		status, body := f.meStatus, f.meBody
		f.mu.Unlock()
		if f.wantCookie != "" && !strings.Contains(r.Header.Get("Cookie"), f.wantCookie) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"not_login"}`))
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"not_login"}`))
			return
		}
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		if body == "" {
			body = `{"id":"user-abcdef123456","name":"Alice","email":"alice@example.com"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	case chatgpt.PathSession:
		f.mu.Lock()
		f.sessCall++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"","user":{"id":"user-abcdef123456"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Model not found"}`))
	}
}

func (f *fakeChatGPT) counts() (me, sess int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.meCalls, f.sessCall
}

// newChatGPTApp 构造只含 ChatGPT runtime 的最小 App；srv 为假上游。
func newChatGPTApp(t *testing.T, f *fakeChatGPT) (*App, *pool.Pool) {
	t.Helper()
	return newChatGPTAppWithHandler(t, f)
}

// newChatGPTAppWithHandler 同 newChatGPTApp，但允许任意假上游 handler
// （导入测试需要按 token 返回不同账号，见 chatgpt_import_test.go）。
func newChatGPTAppWithHandler(t *testing.T, h http.Handler) (*App, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cgPool := pool.New(filepath.Join(dir, "state-chatgpt.json"))
	up := chatgpt.NewWithConfig(chatgpt.Config{Base: srv.URL, DataDir: filepath.Join(dir, "chatgpt-data")})
	t.Cleanup(up.Close)
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.ChatGPT: {Kind: provider.ChatGPT, Pool: cgPool, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, cgPool
}

// testJWT 造一枚形状合法、exp 在未来的 access token（不验签，只解载荷）。
func testJWT(t *testing.T, exp int64, extra string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	payload := map[string]any{
		"exp": exp,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-777",
		},
	}
	if extra != "" {
		payload["https://api.openai.com/profile"] = map[string]any{"email": extra}
	}
	return enc(map[string]any{"alg": "RS256"}) + "." + enc(payload) + ".sig"
}

const testCookie = "__Secure-next-auth.session-token=abc123; cf_clearance=zzz"

// TestSubmitChatGPTAuthSuccess 成功链路：零费用校验 → 落盘 → 立刻进池。
func TestSubmitChatGPTAuthSuccess(t *testing.T) {
	f := &fakeChatGPT{wantCookie: "__Secure-next-auth.session-token"}
	a, cgPool := newChatGPTApp(t, f)
	tok := testJWT(t, time.Now().AddDate(0, 2, 0).Unix(), "")

	uid, warn, err := a.SubmitChatGPTAuth(testCookie, "Bearer "+tok)
	if err != nil {
		t.Fatalf("SubmitChatGPTAuth: %v", err)
	}
	if warn != "" {
		t.Errorf("首次添加不应有告警，得到 %q", warn)
	}
	if uid != "user-abcdef123456" {
		t.Errorf("uid = %q，期望取 /backend-api/me 的 id", uid)
	}
	if me, _ := f.counts(); me != 1 {
		t.Errorf("/backend-api/me 调用 %d 次，期望恰好 1 次（零费用校验只发一次）", me)
	}

	// ① 账号**立刻**进池（面板无需刷新、无需重启）
	if cgPool.AuthByUID(uid) == nil {
		t.Fatal("reloadAccounts 没把新账号放进池里（面板要重启才看得到）")
	}

	// ② 落盘形态：文件名前缀 + Kind + 两段凭据 + 真实到期时刻 + 0600
	path := filepath.Join(a.cfg.AuthDir, "chatgpt-"+uid+".json")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("凭据未落盘到 %s：%v", path, statErr)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("凭据文件权限 = %v，必须仅属主可读（0600）", fi.Mode().Perm())
	}
	list, lerr := auth.LoadChatGPTDir(a.cfg.AuthDir)
	if lerr != nil || len(list) != 1 {
		t.Fatalf("LoadChatGPTDir = %d 个（err=%v），期望 1", len(list), lerr)
	}
	got := list[0]
	if got.Kind != "chatgpt" {
		t.Errorf("Kind = %q，期望 chatgpt（否则 main.go 的 chatgpt-* 加载器扫不到）", got.Kind)
	}
	if got.AccessTokenValue() != tok {
		t.Error("AccessToken 未原样保存")
	}
	if got.Cookies["__Secure-next-auth.session-token"] != "abc123" {
		t.Errorf("Cookie 族未原样保存（只存 access token 会被上游判未登录）：%v", got.Cookies)
	}
	if got.Nickname != "Alice" {
		t.Errorf("昵称 = %q，期望取上游身份 name", got.Nickname)
	}
	if got.ExpiresAt <= time.Now().Unix() {
		t.Errorf("ExpiresAt = %d 必须是真实到期时刻（JWT exp）", got.ExpiresAt)
	}
	if got.Extra["accountId"] != "acct-777" {
		t.Errorf("Extra[accountId] = %q，期望 acct-777（决定 ChatGPT-Account-Id 头）", got.Extra["accountId"])
	}
}

// TestSubmitChatGPTAuthRequiresBothParts 只贴一段必须在**打上游之前**拦下并点名缺哪一段。
func TestSubmitChatGPTAuthRequiresBothParts(t *testing.T) {
	f := &fakeChatGPT{}
	a, cgPool := newChatGPTApp(t, f)
	tok := testJWT(t, time.Now().AddDate(0, 1, 0).Unix(), "")

	cases := []struct {
		name, cookie, auth, wantWord string
	}{
		{"缺 Cookie", "", tok, "Cookie"},
		{"缺 Authorization", testCookie, "", "Authorization"},
	}
	for _, c := range cases {
		_, _, err := a.SubmitChatGPTAuth(c.cookie, c.auth)
		if err == nil {
			t.Errorf("%s: 应报错（半截凭据会产出「看起来成功、每次 403」的账号）", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantWord) {
			t.Errorf("%s: 错误文案必须点名缺失的那一段（%q），得到 %v", c.name, c.wantWord, err)
		}
	}
	if me, _ := f.counts(); me != 0 {
		t.Errorf("输入不全时打了 %d 次上游，应为 0（本地就该拦）", me)
	}
	if list, _ := auth.LoadChatGPTDir(a.cfg.AuthDir); len(list) != 0 || len(cgPool.List()) != 0 {
		t.Error("失败路径不得落盘 / 进池")
	}
}

// TestSubmitChatGPTAuthUpstreamRejection 校验失败：不落盘，且如实回显上游原因（含分类提示）。
func TestSubmitChatGPTAuthUpstreamRejection(t *testing.T) {
	f := &fakeChatGPT{meStatus: 401, meBody: `{"detail":"not_login"}`}
	a, cgPool := newChatGPTApp(t, f)
	tok := testJWT(t, time.Now().AddDate(0, 1, 0).Unix(), "")

	uid, _, err := a.SubmitChatGPTAuth(testCookie, tok)
	if err == nil {
		t.Fatal("上游 401 必须让提交失败（不得落盘）")
	}
	if uid != "" {
		t.Errorf("失败时不应返回 uid，得到 %q", uid)
	}
	if !strings.Contains(err.Error(), "未登录") || !strings.Contains(err.Error(), "同一个已登录会话") {
		t.Errorf("错误文案要能指导用户（两段必须来自同一会话），得到 %v", err)
	}
	if !strings.Contains(err.Error(), "not_login") {
		t.Errorf("必须如实回显上游原因，得到 %v", err)
	}
	if list, _ := auth.LoadChatGPTDir(a.cfg.AuthDir); len(list) != 0 || len(cgPool.List()) != 0 {
		t.Error("校验失败不得落盘 / 进池")
	}
}

// TestSubmitChatGPTAuthNoSecretInMessages 零 token 纪律：返回文案里绝不出现凭据明文。
func TestSubmitChatGPTAuthNoSecretInMessages(t *testing.T) {
	f := &fakeChatGPT{meStatus: 403, meBody: `{"detail":"Unusual activity has been detected from your device"}`}
	a, _ := newChatGPTApp(t, f)
	tok := testJWT(t, time.Now().AddDate(0, 1, 0).Unix(), "")

	_, warn, err := a.SubmitChatGPTAuth(testCookie, tok)
	if err == nil {
		t.Fatal("403 风控必须让提交失败")
	}
	if !strings.Contains(err.Error(), "风控") {
		t.Errorf("403 风控文案要说明「同组凭据别重试、换代理」，得到 %v", err)
	}
	for _, secret := range []string{tok, "abc123", "cf_clearance"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(warn, secret) {
			t.Errorf("返回文案泄漏了凭据片段 %q：err=%v warn=%q", secret, err, warn)
		}
	}
}

// TestSubmitChatGPTAuthRepaste 重复粘贴同一账号：给覆盖告警，且不冲掉用户改过的昵称。
func TestSubmitChatGPTAuthRepaste(t *testing.T) {
	f := &fakeChatGPT{}
	a, cgPool := newChatGPTApp(t, f)
	tok := testJWT(t, time.Now().AddDate(0, 2, 0).Unix(), "")

	uid, _, err := a.SubmitChatGPTAuth(testCookie, tok)
	if err != nil {
		t.Fatalf("首次: %v", err)
	}
	first, _ := auth.LoadChatGPTDir(a.cfg.AuthDir)
	first[0].Nickname = "我的网页号"
	if serr := first[0].SaveAtomic(); serr != nil {
		t.Fatal(serr)
	}

	uid2, warn, err := a.SubmitChatGPTAuth(testCookie, tok)
	if err != nil {
		t.Fatalf("重贴: %v", err)
	}
	if uid2 != uid {
		t.Errorf("同一账号重贴应落同一个 uid，得到 %q / %q", uid, uid2)
	}
	if !strings.Contains(warn, "已经添加过了") {
		t.Errorf("重贴必须告警「覆盖」，得到 %q", warn)
	}
	after, _ := auth.LoadChatGPTDir(a.cfg.AuthDir)
	if len(after) != 1 {
		t.Fatalf("重贴不得新增条目：%d 个", len(after))
	}
	if after[0].Nickname != "我的网页号" {
		t.Errorf("重贴冲掉了用户改的名字：%q", after[0].Nickname)
	}
	if n := len(cgPool.List()); n != 1 {
		t.Errorf("池内账号数 = %d，期望 1", n)
	}
}

// TestChatGPTNicknamePrecedence 昵称优先级：用户改名 > 上游身份 > 默认名（uid 尾号）。
func TestChatGPTNicknamePrecedence(t *testing.T) {
	const uid = "user-abcdef123456"
	me := &chatgpt.UserInfo{ID: uid, Name: "Alice", Email: "alice@example.com"}
	tok := testJWT(t, 0, "jwtmail@example.com")

	if got := chatgptNickname(me, tok, uid, ""); got != "Alice" {
		t.Errorf("无旧昵称时应取上游 name，得到 %q", got)
	}
	if got := chatgptNickname(&chatgpt.UserInfo{ID: uid, Email: "alice@example.com"}, tok, uid, ""); got != "alice@example.com" {
		t.Errorf("上游 name 缺失时应退回 email，得到 %q", got)
	}
	if got := chatgptNickname(&chatgpt.UserInfo{ID: uid}, tok, uid, ""); got != "jwtmail@example.com" {
		t.Errorf("上游身份全缺时应退回 JWT 里的 email，得到 %q", got)
	}
	if got := chatgptNickname(&chatgpt.UserInfo{ID: uid}, "", uid, ""); got != "ChatGPT Web "+uid[len(uid)-6:] {
		t.Errorf("全缺时应给默认名「ChatGPT Web <尾号>」，得到 %q", got)
	}
	// 用户改过的名字优先于上游身份（不能每次重贴都把名字洗回上游值）
	if got := chatgptNickname(me, tok, uid, "我的名字"); got != "我的名字" {
		t.Errorf("用户改过的名字必须保留，得到 %q", got)
	}
	// 旧昵称恰好等于默认名 = 没改过，应被上游身份刷新
	if got := chatgptNickname(me, tok, uid, "ChatGPT Web "+uid[len(uid)-6:]); got != "Alice" {
		t.Errorf("旧昵称是默认名时应更新为上游身份，得到 %q", got)
	}
}

// TestSubmitChatGPTAuthNoUID 上游连 id 都不给时必须失败：编个 uid 会让同一账号每次添加都成新条目。
func TestSubmitChatGPTAuthNoUID(t *testing.T) {
	f := &fakeChatGPT{meBody: `{"name":"NoID"}`}
	a, _ := newChatGPTApp(t, f)
	// 无 JWT 载荷（不是合法 JWT）⇒ uid 无从得出
	if uid, _, err := a.SubmitChatGPTAuth(testCookie, "opaque-token-value"); err == nil {
		t.Errorf("拿不到账号 ID 时必须失败，得到 uid=%q", uid)
	}
	if list, _ := auth.LoadChatGPTDir(a.cfg.AuthDir); len(list) != 0 {
		t.Error("拿不到 uid 不得落盘")
	}
}

// TestChatGPTAuthErrorPassthrough 其它 403（地区/代理）要点名「多半是没配代理」。
func TestChatGPTAuthErrorPassthrough(t *testing.T) {
	err := chatgptAuthError(&provider.Error{Kind: provider.ErrPassthrough, Status: 403, Msg: "boom"})
	if !strings.Contains(err.Error(), "代理") {
		t.Errorf("403 非风控必须提示配代理（spec §15 代价披露），得到 %v", err)
	}
	if e := chatgptAuthError(fmt.Errorf("dial tcp: i/o timeout")); !strings.Contains(e.Error(), "代理") {
		t.Errorf("传输层失败也要提示代理，得到 %v", e)
	}
}

// TestRouteChatGPTAuthPaste 端到端：面板 POST /api/login/chatgpt_auth 成功即入池，
// 且 /api/state 只回昵称与 uid —— **不回显任何凭据明文**（票 05 验收第 3 条）。
func TestRouteChatGPTAuthPaste(t *testing.T) {
	f := &fakeChatGPT{wantCookie: "__Secure-next-auth.session-token"}
	a, cgPool := newChatGPTApp(t, f)
	mux := http.NewServeMux()
	a.HandleAPI(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tok := testJWT(t, time.Now().AddDate(0, 2, 0).Unix(), "")
	body, _ := json.Marshal(map[string]string{"cookie": testCookie, "authorization": "Bearer " + tok})
	resp, err := http.Post(srv.URL+"/api/login/chatgpt_auth", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，want 200：%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if jerr := json.Unmarshal(raw, &out); jerr != nil {
		t.Fatalf("响应不是 JSON：%s", raw)
	}
	if out["ok"] != true || out["uid"] != "user-abcdef123456" {
		t.Errorf("响应 = %s，期望 ok=true 且带 uid", raw)
	}
	for _, secret := range []string{tok, "abc123", "cf_clearance"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("提交响应里回显了凭据片段 %q", secret)
		}
	}

	// 账号页数据（/api/state）：有昵称有 uid，没有任何凭据字段
	if cgPool.AuthByUID("user-abcdef123456") == nil {
		t.Fatal("账号未进池")
	}
	stateResp, serr := http.Get(srv.URL + "/api/state")
	if serr != nil {
		t.Fatalf("GET /api/state: %v", serr)
	}
	defer stateResp.Body.Close()
	stateRaw, _ := io.ReadAll(stateResp.Body)
	for _, secret := range []string{tok, "abc123", "cf_clearance"} {
		if strings.Contains(string(stateRaw), secret) {
			t.Errorf("/api/state 回显了凭据片段 %q（30s 轮询接口绝不能下发明文）", secret)
		}
	}
	if !strings.Contains(string(stateRaw), "user-abcdef123456") || !strings.Contains(string(stateRaw), "Alice") {
		t.Errorf("/api/state 里找不到新账号的 uid/nickname")
	}

	// 失败分支：上游 401 必须 400 + 可读原因
	f2 := &fakeChatGPT{meStatus: 401, meBody: `{"detail":"not_login"}`}
	a2, _ := newChatGPTApp(t, f2)
	mux2 := http.NewServeMux()
	a2.HandleAPI(mux2)
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	resp2, err2 := http.Post(srv2.URL+"/api/login/chatgpt_auth", "application/json", strings.NewReader(string(body)))
	if err2 != nil {
		t.Fatalf("POST(2): %v", err2)
	}
	defer resp2.Body.Close()
	raw2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("上游失败时 status = %d，want 400", resp2.StatusCode)
	}
	if !strings.Contains(string(raw2), "未登录") {
		t.Errorf("失败响应要回显可读原因，得到 %s", raw2)
	}
}

// TestChatGPTLoginFlowDeclaresNoBrowserFlow 票 05 验收第 4 条：
// 登录流程表里 ChatGPT 必须登记成「无浏览器登录流程，请粘贴或导入」——
// 且**恰好一种**发起方式（Start / DisplayURL / noFlowReason 三选一，init 自检会 panic）。
func TestChatGPTLoginFlowDeclaresNoBrowserFlow(t *testing.T) {
	f, ok := loginFlows[provider.ChatGPT]
	if !ok || f == nil {
		t.Fatal("loginFlows 里没有 ChatGPT（前端「添加账号 / ⟳」会落到 unknown login provider）")
	}
	if f.Start != nil || f.Poll != nil {
		t.Error("ChatGPT 没有浏览器登录编排（凭据只能粘贴/导入），不得登记 Start/Poll")
	}
	if f.noFlowReason == "" {
		t.Fatal("必须写明 noFlowReason（三选一之一），否则 init 自检 panic")
	}
	for _, word := range []string{"粘贴", "无浏览器登录流程", "代理"} {
		if !strings.Contains(f.noFlowReason, word) {
			t.Errorf("说明文案应含 %q，得到 %q", word, f.noFlowReason)
		}
	}
}

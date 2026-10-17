package login_lobsterai

// login_test.go 回归测试：锁定 LobsterAI 本机回调登录的易错点。
//
// 全部走**本机 httptest / 127.0.0.1 回调口**，**绝不**指向生产上游
// （AGENTS.md §0.1：严禁穷举/批量探测上游接口）：
//  1. 本机回调 server：只绑 127.0.0.1、state 原样比对（不匹配 400 拒绝）；
//  2. exchange 请求形状：路径 / 5 字段体 / **不带 Authorization** / UA 取自兄弟包 / version 来源；
//  3. 响应信封：code=0 + data 必须是对象 + 缺 accessToken 即报错（错误文案零 token）；
//  4. uid 四级回退（user.id → user.userId → user.yid → sha256(accessToken)[:16]，**无 JWT sub**）；
//  5. expires_at：expiresIn 优先，缺失回退 JWT exp，都没有留空；
//  6. SaveAuth/LoadLobsterAIDir 往返（含 Extra 的 keyfrom 四元组）+ glob 前缀边界。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/lobsterai"
)

// withTestEndpoint 把包内 endpointHost / clientVersionFn 指向测试替身，测试结束恢复。
// 两者都必须替换：前者决定 exchange 打哪里，后者默认会去打真实的版本号接口。
func withTestEndpoint(t *testing.T, host, version string) {
	t.Helper()
	oldHost, oldVersion := endpointHost, clientVersionFn
	if host != "" {
		endpointHost = host
	}
	if version != "" {
		clientVersionFn = func() string { return version }
	}
	t.Cleanup(func() { endpointHost, clientVersionFn = oldHost, oldVersion })
}

// startTestLogin 起一条登录流程并注册清理（回调口必须关，否则测试间端口泄漏）。
func startTestLogin(t *testing.T, statePath string) state {
	t.Helper()
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(Shutdown)
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return st
}

// TestStartLoginURLAndLoopback 校验登录 URL 形态、state/uuid/keyfrom 生成、以及回调口绑定。
func TestStartLoginURLAndLoopback(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	st := startTestLogin(t, statePath)

	// 登录 URL = {PortalBase}/portal#/login?source=electron&redirect_uri=...&state=...
	if !strings.HasPrefix(st.AuthURL, lobsterai.PortalBase+"/portal#/login?") {
		t.Fatalf("登录 URL 前缀不对: %q", st.AuthURL)
	}
	// query 在 `#` 之后（文档字面形态）；用 url.Parse 拆不出来，故手工切。
	q := st.AuthURL[strings.Index(st.AuthURL, "?")+1:]
	vals, err := url.ParseQuery(q)
	if err != nil {
		t.Fatalf("登录 URL query 解析失败: %v", err)
	}
	if got := vals.Get("source"); got != loginSource {
		t.Fatalf("source = %q, want %q", got, loginSource)
	}
	if got, want := vals.Get("state"), st.State; got != want {
		t.Fatalf("URL state = %q, want %q", got, want)
	}
	wantRedirect := "http://127.0.0.1:" + itoa(st.CallbackPort) + "/auth/callback"
	if got := vals.Get("redirect_uri"); got != wantRedirect {
		t.Fatalf("redirect_uri = %q, want %q", got, wantRedirect)
	}
	if st.State == "" || st.LoginUUID == "" || st.State == st.LoginUUID {
		t.Fatalf("state / 安装 UUID 生成异常: state=%q uuid=%q", st.State, st.LoginUUID)
	}
	// firstKeyfrom = latestKeyfrom = 登录时刻的毫秒字符串
	if st.FirstKeyfrom == "" || st.FirstKeyfrom != st.LatestKeyfrom {
		t.Fatalf("keyfrom 不符: %q / %q", st.FirstKeyfrom, st.LatestKeyfrom)
	}
	if ms, err := strconv.ParseInt(st.FirstKeyfrom, 10, 64); err != nil || ms < 1e12 {
		t.Fatalf("firstKeyfrom 不是毫秒串: %q (err=%v)", st.FirstKeyfrom, err)
	}

	// 回调口只绑 127.0.0.1（**绝不** 0.0.0.0）
	sess := currentSession()
	if sess == nil || sess.ln == nil {
		t.Fatal("回调 server 未安装")
	}
	if got := sess.ln.Addr().String(); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Fatalf("回调口监听地址 = %q, want 127.0.0.1:*", got)
	}
}

// TestCallbackStateValidation 是本包最关键的安全回归：state 不匹配必须 400 拒绝且不写入 code。
// 同时锁定「拒绝即终态」的取舍：错误记入 pending 让 Poll 立刻报出可行动的错误，
// 而不是傻等 10 分钟（state 错的回调永远不可能变成对的回调）。
func TestCallbackStateValidation(t *testing.T) {
	cases := []struct {
		name       string
		stateQ     string
		wantCode   int
		wantConclu bool
	}{
		{"state 缺失", "", http.StatusBadRequest, true},
		{"state 不匹配", "not-the-state", http.StatusBadRequest, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			st := startTestLogin(t, filepath.Join(dir, "login-state.json"))
			sess := currentSession()
			base := "http://" + sess.ln.Addr().String()

			resp, err := http.Get(base + callbackPath + "?code=SECRET-CODE&state=" + url.QueryEscape(tc.stateQ))
			if err != nil {
				t.Fatalf("回调请求失败: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
			code, errMsg, got := sess.snapshot()
			if code != "" {
				t.Fatalf("state 校验失败却收下了 code: %q（state=%q）", code, st.State)
			}
			if got != tc.wantConclu {
				t.Fatalf("pending.received = %v, want %v", got, tc.wantConclu)
			}
			if tc.wantConclu && errMsg == "" {
				t.Fatal("应记录拒绝原因")
			}
		})
	}

	// 正向：state 匹配 → 200 且收下 code
	dir := t.TempDir()
	st := startTestLogin(t, filepath.Join(dir, "login-state.json"))
	sess := currentSession()
	resp, err := http.Get("http://" + sess.ln.Addr().String() + callbackPath +
		"?code=SECRET-CODE&state=" + url.QueryEscape(st.State))
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if code, _, _ := sess.snapshot(); code != "SECRET-CODE" {
		t.Fatalf("授权码未收下: %q", code)
	}
	if !strings.Contains(string(body), "已收到授权") {
		t.Fatalf("成功页文案不符（刻意不写「登录成功」以免兑换失败时误导）: %s", string(body))
	}
}

// TestExchangeRequestAndPollSuccess 校验 exchange 的请求形状与一次完整 Poll 成功路径。
func TestExchangeRequestAndPollSuccess(t *testing.T) {
	var gotPath, gotUA, gotAuthz, gotCT string
	var gotBody map[string]string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotPath, gotUA = r.URL.Path, r.Header.Get("User-Agent")
		gotAuthz, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{
			"accessToken":"at-1","refreshToken":"rt-1","expiresIn":7200,
			"user":{"id":12345,"yid":"yid-9","userId":"uid-9","nickname":"王**"}}}`))
	}))
	defer srv.Close()
	withTestEndpoint(t, srv.URL, "2026.9.9")

	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	st := startTestLogin(t, statePath)

	// 模拟浏览器回调（state 正确）
	resp, err := http.Get("http://" + currentSession().ln.Addr().String() + callbackPath +
		"?code=CODE-1&state=" + url.QueryEscape(st.State))
	if err != nil {
		t.Fatalf("回调失败: %v", err)
	}
	_ = resp.Body.Close()

	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if calls != 1 {
		t.Fatalf("exchange 应恰好打 1 次，got %d", calls)
	}
	if gotPath != exchangePath {
		t.Fatalf("路径 = %q, want %q", gotPath, exchangePath)
	}
	if gotAuthz != "" {
		t.Fatalf("exchange **不得**带 Authorization，got %q", gotAuthz)
	}
	if gotUA != lobsterai.UserAgent {
		t.Fatalf("User-Agent = %q, want %q（必须取自兄弟包常量）", gotUA, lobsterai.UserAgent)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Fatalf("Content-Type = %q", gotCT)
	}
	// 体必须恰好 5 个字段（authCode/firstKeyfrom/latestKeyfrom/uuid/version）
	if len(gotBody) != 5 {
		t.Fatalf("exchange 体字段数 = %d, want 5: %v", len(gotBody), gotBody)
	}
	if gotBody["authCode"] != "CODE-1" {
		t.Fatalf("authCode = %q", gotBody["authCode"])
	}
	if gotBody["firstKeyfrom"] != st.FirstKeyfrom {
		t.Fatalf("firstKeyfrom = %q, want %q", gotBody["firstKeyfrom"], st.FirstKeyfrom)
	}
	if gotBody["uuid"] != st.LoginUUID {
		t.Fatalf("uuid = %q, want %q", gotBody["uuid"], st.LoginUUID)
	}
	if gotBody["version"] != "2026.9.9" {
		t.Fatalf("version = %q, want 兄弟包下发的动态值", gotBody["version"])
	}
	// latestKeyfrom 是**当前时刻**（与凭据里持久化的那份不同口径）
	if gotBody["latestKeyfrom"] == "" || gotBody["latestKeyfrom"] == st.LatestKeyfrom {
		// 毫秒级并发下理论上可能相等，但概率极低；这里只在明显等于登录时刻时告警。
		t.Logf("latestKeyfrom = %q（登录时刻=%q）", gotBody["latestKeyfrom"], st.LatestKeyfrom)
	}

	// Result 字段
	if res.AccessToken != "at-1" || res.RefreshToken != "rt-1" {
		t.Fatalf("token 解析错误: %+v", res)
	}
	if res.UID != "12345" { // user.id 优先（数字形态也要认）
		t.Fatalf("UID = %q, want 12345（user.id 优先）", res.UID)
	}
	if res.UserID != "yid-9" {
		t.Fatalf("UserID（有道 yid）= %q, want yid-9", res.UserID)
	}
	if res.Nickname != "王**" || res.Domain != domainConst || res.ApiHost != lobsterai.APIBase {
		t.Fatalf("Domain/ApiHost/Nickname 不符: %+v", res)
	}
	if res.ExpiresIn != 7200 {
		t.Fatalf("ExpiresIn = %d", res.ExpiresIn)
	}
	if d := res.ExpiresAt - time.Now().Unix(); d < 7195 || d > 7200 {
		t.Fatalf("ExpiresAt 与 now+expiresIn 不符: delta=%d", d)
	}
	if res.UUID != st.LoginUUID || res.FirstKeyfrom != st.FirstKeyfrom || res.LatestKeyfrom != st.LatestKeyfrom {
		t.Fatalf("keyfrom 四元组未随 Result 带出: %+v", res)
	}
	// 成功后 state 文件与回调口都应清理
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("登录成功后 state 文件应被删除")
	}
	if currentSession() != nil {
		t.Fatal("登录成功后回调 server 应已关闭")
	}
}

// TestParseExchangeEnvelope 覆盖信封硬校验与"错误文案零 token"。
func TestParseExchangeEnvelope(t *testing.T) {
	st := state{LoginUUID: "u", FirstKeyfrom: "1", LatestKeyfrom: "1"}
	cases := []struct {
		name   string
		status int
		raw    string
		want   string // 期望错误文案里出现的子串
	}{
		{"code 非 0", 200, `{"code":10001,"msg":"auth code expired","data":null}`, "code=10001"},
		{"data 为 null", 200, `{"code":0,"msg":"ok","data":null}`, "缺 data"},
		{"data 缺失", 200, `{"code":0,"msg":"ok"}`, "缺 data"},
		{"data 非对象", 200, `{"code":0,"msg":"ok","data":123}`, "不是对象"},
		{"缺 accessToken", 200, `{"code":0,"msg":"ok","data":{"refreshToken":"rt","user":{"id":"u"}}}`, "accessToken"},
		{"非 JSON", 200, `<html>proxy</html>`, "不是 JSON"},
		{"http 400", 400, `{"code":7,"msg":"bad request","data":{"accessToken":"LEAK"}}`, "http 400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseExchange(tc.status, []byte(tc.raw), st)
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误文案 = %q, want 含 %q", err.Error(), tc.want)
			}
			// 零 token 纪律：任何错误文案都不得回显 data 里的凭据原文
			if strings.Contains(err.Error(), "LEAK") {
				t.Fatalf("错误文案回显了响应体中的凭据: %q", err.Error())
			}
		})
	}
}

// TestUIDFourLevelFallback 覆盖 uid 四级回退，并确认**不插 JWT sub**。
func TestUIDFourLevelFallback(t *testing.T) {
	// 与 parseExchange 内部 data 结构同形；连带被解析的 user 字段做端到端校验。
	type user struct {
		ID       json.RawMessage `json:"id"`
		YID      json.RawMessage `json:"yid"`
		UserID   json.RawMessage `json:"userId"`
		Nickname string          `json:"nickname"`
	}
	type data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		User         user   `json:"user"`
	}
	const at = "at-xyz"
	sum := sha256.Sum256([]byte(at))
	wantHash := hex.EncodeToString(sum[:])[:16]

	cases := []struct {
		name string
		user string
		want string
	}{
		{"① user.id", `{"id":"id-1","yid":"yid-1","userId":"uid-1"}`, "id-1"},
		{"① user.id 数字形态", `{"id":998877,"yid":"yid-1"}`, "998877"},
		{"② user.userId", `{"yid":"yid-1","userId":"uid-1"}`, "uid-1"},
		{"③ user.yid", `{"yid":"yid-1"}`, "yid-1"},
		{"④ sha256(accessToken)[:16]", `{}`, wantHash},
		{"空串/空对象都要穿透", `{"id":"","yid":""}`, wantHash},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d data
			if err := json.Unmarshal([]byte(`{"accessToken":"`+at+`","refreshToken":"rt","expiresIn":60,"user":`+tc.user+`}`), &d); err != nil {
				t.Fatal(err)
			}
			got := deriveUID(d.User.ID, d.User.UserID, d.User.YID, json.RawMessage(d.AccessToken))
			if got != tc.want {
				t.Fatalf("UID = %q, want %q", got, tc.want)
			}
			// 刻意**不插入** JWT sub：即使 accessToken 里带 sub，也只能落到第 4 级哈希
			if strings.HasPrefix(at, "eyJ") && got == "sub-value" {
				t.Fatal("不得插入 JWT sub 一级")
			}
		})
	}

	// 显式反例：JWT 里带 sub，但 user 字段全空 ⇒ 仍走 sha256 哈希，而不是 sub。
	jwt := makeUnsignedJWT(`{"sub":"sub-value","exp":` + itoa64(time.Now().Add(time.Hour).Unix()) + `}`)
	got := deriveUID(nil, nil, nil, json.RawMessage(jwt))
	if got == "sub-value" {
		t.Fatal("uid 回退里不得出现 JWT sub")
	}
	jsum := sha256.Sum256([]byte(jwt))
	if got != hex.EncodeToString(jsum[:])[:16] {
		t.Fatalf("UID = %q, want sha256 前 16 字符", got)
	}
}

// TestExpiresAtFallbacks 覆盖 expires_at 的三级：expiresIn → JWT exp → 留空。
func TestExpiresAtFallbacks(t *testing.T) {
	// ① expiresIn（秒）优先
	if got := expiresAtSeconds(3600, ""); got-time.Now().Unix() < 3595 || got-time.Now().Unix() > 3600 {
		t.Fatalf("expiresIn 分支偏差: %d", got)
	}
	// ② 缺失时回退 JWT exp（秒级 → 秒）
	exp := time.Now().Add(2 * time.Hour).Unix()
	jwt := makeUnsignedJWT(`{"exp":` + itoa64(exp) + `}`)
	if got := expiresAtSeconds(0, jwt); got != exp {
		t.Fatalf("JWT 回退 = %d, want %d", got, exp)
	}
	// 毫秒级 exp 也要认
	jwtMs := makeUnsignedJWT(`{"exp":` + itoa64(exp*1000) + `}`)
	if got := expiresAtSeconds(0, jwtMs); got != exp {
		t.Fatalf("毫秒级 JWT exp 回退 = %d, want %d", got, exp)
	}
	// ③ 都拿不到 → 0（留空，面板据此不显示到期时间）
	if got := expiresAtSeconds(0, "not-a-jwt"); got != 0 {
		t.Fatalf("无来源时应留空（0），got %d", got)
	}
	if got := expiresAtSeconds(0, makeUnsignedJWT(`{"sub":"x"}`)); got != 0 {
		t.Fatalf("JWT 无 exp 时应留空（0），got %d", got)
	}
}

// TestSaveAuthRoundTripAndGlobBoundary 校验落盘 / 加载往返、Extra keyfrom 四元组、glob 前缀边界。
func TestSaveAuthRoundTripAndGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	r := Result{
		AccessToken:   "at-x",
		RefreshToken:  "rt-y",
		ExpiresAt:     time.Now().Add(time.Hour).Unix(),
		UID:           "12345",
		Nickname:      "王**",
		Domain:        domainConst,
		ApiHost:       lobsterai.APIBase,
		UUID:          "uuid-install",
		FirstKeyfrom:  "1700000000000",
		LatestKeyfrom: "1700000000000",
		UserID:        "yid-9",
	}
	fp, err := SaveAuth(dir, r)
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	if filepath.Base(fp) != "lobsterai-12345.json" {
		t.Fatalf("文件名 = %q", filepath.Base(fp))
	}
	// 磁盘形态必须是 auth 包的嵌套形（不变式 3）：auth.Parse 能直接读
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["auth"]; !ok {
		t.Fatalf("落盘不是嵌套形: %s", string(raw))
	}
	if _, ok := probe["account"]; !ok {
		t.Fatalf("落盘缺 account 段: %s", string(raw))
	}

	// 借道"更宽前缀"渠道文件：content 合法但**不得**被 LoadLobsterAIDir 吞进来
	broad := filepath.Join(dir, "lobsterai2-other.json")
	if err := os.WriteFile(broad, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	auths, err := auth.LoadLobsterAIDir(dir)
	if err != nil {
		t.Fatalf("LoadLobsterAIDir: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("LoadLobsterAIDir 收到 %d 个账号，want 1（lobsterai2- 不得被吞）", len(auths))
	}
	a := auths[0]
	if a.Kind != "lobsterai" || a.UID != r.UID || a.AccessToken != r.AccessToken || a.RefreshToken != r.RefreshToken {
		t.Fatalf("往返后字段不符: %+v", a)
	}
	if a.ExpiresAt != r.ExpiresAt || a.FilePath != fp || a.Nickname != r.Nickname {
		t.Fatalf("往返后 expiresAt/FilePath/nickname 不符: %+v", a)
	}
	if a.ApiHost != lobsterai.APIBase {
		t.Fatalf("ApiHost = %q", a.ApiHost)
	}
	// keyfrom 四元组必须原样活下来（少了它账号在 refresh / 模型列表上直接不可用）
	for k, want := range map[string]string{
		extraKeyUUID:          r.UUID,
		extraKeyFirstKeyfrom:  r.FirstKeyfrom,
		extraKeyLatestKeyfrom: r.LatestKeyfrom,
		extraKeyUserID:        r.UserID,
	} {
		if got := a.Extra[k]; got != want {
			t.Fatalf("Extra[%s] = %q, want %q（全部 Extra=%v）", k, got, want, a.Extra)
		}
	}

	// 缺 accessToken / uid 一律拒（不落半成品凭据）
	if _, err := SaveAuth(dir, Result{UID: "u", AccessToken: ""}); err == nil {
		t.Fatal("缺 accessToken 应被拒绝")
	}
	if _, err := SaveAuth(dir, Result{UID: "", AccessToken: "a"}); err == nil {
		t.Fatal("缺 uid 应被拒绝")
	}
	// uid 含文件系统危险字符时必须消毒
	fp2, err := SaveAuth(dir, Result{UID: "../evil", AccessToken: "a"})
	if err != nil {
		t.Fatalf("SaveAuth(危险 uid): %v", err)
	}
	if filepath.Dir(fp2) != dir {
		t.Fatalf("危险 uid 逃出了 authDir: %q", fp2)
	}
}

// TestCallbackPageRendering 校验回调页占位符全部替换 + 错误文案被 HTML 转义。
func TestCallbackPageRendering(t *testing.T) {
	okPage := callbackPage(true, "")
	if strings.Contains(okPage, "%!") {
		t.Fatal("成功页含未替换占位符")
	}
	if !strings.Contains(okPage, "已收到授权") || !strings.Contains(okPage, "autoClose = true") {
		t.Fatal("成功页缺少文案或自动关闭脚本")
	}
	errPage := callbackPage(false, `<script>alert(1)</script> & "q"`)
	if strings.Contains(errPage, "%!") {
		t.Fatal("失败页含未替换占位符")
	}
	if strings.Contains(errPage, "<script>alert") {
		t.Fatal("错误文案被原样嵌入，存在 XSS")
	}
	if !strings.Contains(errPage, "&lt;script&gt;") {
		t.Fatal("错误文案未做 HTML 转义")
	}
}

// TestPollTimeoutIsTenMinutes 校验流程超时口径（备忘 110 行：10 分钟）。
func TestPollTimeoutIsTenMinutes(t *testing.T) {
	if flowTTL != 10*time.Minute {
		t.Fatalf("flowTTL = %v, want 10m", flowTTL)
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	st := startTestLogin(t, statePath)
	// 把 CreatedAt 拨回 11 分钟前 ⇒ Poll 必须按超时终态处理
	st.CreatedAt = time.Now().Add(-11 * time.Minute).Unix()
	if err := writeState(statePath, st); err != nil {
		t.Fatal(err)
	}
	_, err := Poll(NewClient(), statePath)
	if err == nil || errors.Is(err, ErrPending) || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("超时轮询应返回超时错误，got %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("超时后 state 文件应被清理")
	}
}

// ---------------------------------------------------------------------------
// 测试工具
// ---------------------------------------------------------------------------

// makeUnsignedJWT 造一个"样子对但无签名"的 JWT（只用于解析测试，不发任何请求）。
func makeUnsignedJWT(payload string) string {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"none","typ":"JWT"}`) + "." + enc(payload) + "." + enc("sig")
}

// itoa / itoa64 是不引 strconv 的小整数格式化（测试里只为拼 JSON 用）。
func itoa(n int) string { return itoa64(int64(n)) }

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

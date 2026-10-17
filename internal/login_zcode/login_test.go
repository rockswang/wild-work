// login_test.go login_zcode 的回归测试。
//
// 纪律：**全部走 httptest 假上游，零真实网络**（AGENTS.md §0.1）。
// 覆盖点（对应任务要求）：pending / denied / expired 三个分支、4xx 终态 + 固化、
// 5xx 继续轮询、ready 字段硬校验、SaveAuth 的字段约定与 glob 边界。
package login_zcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// useServer 把包级 endpointOrigin 指向假上游，并在用例结束时恢复。
func useServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	orig := endpointOrigin
	endpointOrigin = srv.URL
	t.Cleanup(func() {
		endpointOrigin = orig
		srv.Close()
	})
	return srv
}

// resetThrottle 把 state 的 NextPollAt 清零，让下一次 Poll 真的去问上游。
//
// 生产路径上 app.pollLogin 是 2s 一跳、由 interval 节流；测试要立即轮询就必须显式清掉
// 节流时间戳（这也顺带证明节流字段确实生效，见 TestLoginPollThrottleLocal）。
func resetThrottle(t *testing.T, statePath string) {
	t.Helper()
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	st.NextPollAt = 0
	if err := writeState(statePath, st); err != nil {
		t.Fatalf("writeState: %v", err)
	}
}

func initResponse(flowID string) string {
	return fmt.Sprintf(`{"code":0,"msg":"","data":{"flow_id":%q,"poll_token":"pt-1",`+
		`"authorize_url":"https://bigmodel.cn/login?appId=zcode&state=abc",`+
		`"expires_at":%d,"poll_interval_sec":2}}`, flowID, time.Now().Add(5*time.Minute).Unix())
}

// ---------------------------------------------------------------------------
// 正常流程
// ---------------------------------------------------------------------------

func TestLoginReadyFlow(t *testing.T) {
	var (
		pollHits   int
		initHdr    http.Header
		pollHdr    http.Header
		initBody   map[string]any
		initCalled bool
	)
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == initPath:
			initCalled = true
			initHdr = r.Header.Clone()
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &initBody)
			_, _ = w.Write([]byte(initResponse("flow-1")))
		case r.Method == http.MethodGet && r.URL.Path == pollPath+"flow-1":
			pollHits++
			pollHdr = r.Header.Clone()
			if pollHits == 1 {
				_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"ready","token":"zcode.jwt.value",
				"user":{"user_id":"12345678901234567","name":"张三"},
				"zai":{"access_token":"zai-token","refresh_token":"zai-rt"}}}`))
		default:
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")

	url, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if url != "https://bigmodel.cn/login?appId=zcode&state=abc" {
		t.Errorf("authorize_url = %q", url)
	}
	if !initCalled {
		t.Fatal("Start 未调用 init 端点")
	}
	// ① 的请求头：会话密钥当 Bearer（32 字节 hex）+ 官方来源头。
	authz := initHdr.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer ") || len(strings.TrimPrefix(authz, "Bearer ")) != 64 {
		t.Errorf("init 的 Bearer 应是 64 位 hex 会话密钥，实际 %q", authz)
	}
	if initHdr.Get("X-ZCode-App-Version") == "" || initHdr.Get("HTTP-Referer") == "" ||
		initHdr.Get("X-Platform") != "win32" || initHdr.Get("User-Agent") == "" {
		t.Errorf("init 头族不全：%v", initHdr)
	}
	if got := initBody["provider"]; got != "bigmodel" {
		t.Errorf("init body.provider = %v，期望 bigmodel", got)
	}

	// state 落盘且带设备标识（UUID 形态）。
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if len(strings.Split(st.DeviceMid, "-")) != 5 {
		t.Errorf("device_mid 应是 UUID 形态：%q", st.DeviceMid)
	}

	// 第一次轮询：pending（且是被**真实请求**换来的）。
	resetThrottle(t, statePath)
	if _, err := Poll(NewClient(), statePath); !errors.Is(err, ErrPending) {
		t.Fatalf("首次轮询应 ErrPending，实际 %v", err)
	}
	if pollHits != 1 {
		t.Fatalf("poll 请求数 = %d，期望 1", pollHits)
	}
	if pollHdr.Get("Authorization") == "" || pollHdr.Get("X-ZCode-App-Version") == "" {
		t.Errorf("poll 头族不全：%v", pollHdr)
	}

	// 第二次轮询：ready。
	resetThrottle(t, statePath)
	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.AccessToken != "zcode.jwt.value" {
		t.Errorf("AccessToken = %q", res.AccessToken)
	}
	if res.UID != "12345678901234567" {
		t.Errorf("UID = %q", res.UID)
	}
	if res.Nickname != "张三" {
		t.Errorf("Nickname = %q", res.Nickname)
	}
	if res.RefreshToken != "" {
		t.Error("本渠道不可续期 ⇒ RefreshToken 必须为空")
	}
	if res.ExpiresAt <= time.Now().AddDate(50, 0, 0).Unix() {
		t.Errorf("ExpiresAt 必须是远期值（否则 NeedsRefresh 恒真），实际 %d", res.ExpiresAt)
	}
	if res.DeviceMid != st.DeviceMid {
		t.Error("凭据里的 device_mid 必须与登录时生成的一致（服务端按它归集用量）")
	}
	if res.ZaiAccessToken != "zai-token" || res.ZaiRefreshToken != "zai-rt" {
		t.Errorf("第三方备用凭据未带上：%+v", res)
	}
	// 成功即删 state（一次性状态）。
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Error("成功后应删除 state 文件")
	}

	// ---- SaveAuth：文件名、字段约定、可被 LoadZCodeDir 读回 ----
	path, err := SaveAuth(dir, res)
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	if filepath.Base(path) != "zcode-12345678901234567.json" {
		t.Errorf("文件名 = %q", filepath.Base(path))
	}
	loaded, err := auth.LoadZCodeDir(dir)
	if err != nil {
		t.Fatalf("LoadZCodeDir: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadZCodeDir 条数 = %d", len(loaded))
	}
	a := loaded[0]
	if a.Kind != "zcode" || a.AccessToken != "zcode.jwt.value" || a.UID != res.UID {
		t.Errorf("读回的凭据不符：%+v", a)
	}
	if a.RefreshToken != "" {
		t.Error("落盘 refreshToken 必须为空")
	}
	if a.Extra["device_mid"] != st.DeviceMid {
		t.Errorf("Extra.device_mid = %q", a.Extra["device_mid"])
	}
	if a.Extra["app_version"] == "" || a.Extra["source"] != "plugin" {
		t.Errorf("Extra 应含 app_version / source：%v", a.Extra)
	}
	// 跨包约定：一等字段不重复塞进 Extra（避免同一事实两处漂移）。
	for _, forbidden := range []string{"access_token", "uid", "nickname", "expires_at"} {
		if _, has := a.Extra[forbidden]; has {
			t.Errorf("Extra 不得重复存放一等字段 %q", forbidden)
		}
	}
	// 同一账号重复保存必须覆盖同一文件（不是每次登录多一个文件）。
	if _, err := SaveAuth(dir, res); err != nil {
		t.Fatalf("SaveAuth 第二次: %v", err)
	}
	loaded, _ = auth.LoadZCodeDir(dir)
	if len(loaded) != 1 {
		t.Errorf("重复保存应覆盖同一文件，实际 %d 个", len(loaded))
	}
}

// TestLoginGlobBoundary 加载器的 glob 边界：`zcode-*.json` 不得吞宽前缀文件。
func TestLoginGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	res := Result{
		AccessToken: "jwt", UID: "u1", Nickname: "n", DeviceMid: "mid",
		ExpiresAt: time.Now().AddDate(100, 0, 0).Unix(),
	}
	if _, err := SaveAuth(dir, res); err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	// 诱饵：`zcode2-*.json` 之类的宽前缀渠道文件不该被本加载器收走。
	decoy := filepath.Join(dir, "zcode2-other.json")
	if err := os.WriteFile(decoy, []byte(`{"auth":{"accessToken":"other"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 另一个诱饵：无 `-` 分隔（`zcodedump.json`）。
	if err := os.WriteFile(filepath.Join(dir, "zcodedump.json"), []byte(`{"auth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := auth.LoadZCodeDir(dir)
	if err != nil {
		t.Fatalf("LoadZCodeDir: %v", err)
	}
	if len(got) != 1 || got[0].UID != "u1" {
		t.Fatalf("glob 边界不符：%+v", got)
	}
}

// ---------------------------------------------------------------------------
// pending / denied / expired
// ---------------------------------------------------------------------------

func TestLoginPollThrottleLocal(t *testing.T) {
	hits := 0
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(initResponse("flow-throttle")))
			return
		}
		hits++
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
	}))
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 服务端下发 interval=2s ⇒ 立刻连续 Poll 两次都应被本地节流拦下，零上游请求。
	for i := 0; i < 2; i++ {
		if _, err := Poll(NewClient(), statePath); !errors.Is(err, ErrPending) {
			t.Fatalf("第 %d 次轮询应 ErrPending，实际 %v", i+1, err)
		}
	}
	if hits != 0 {
		t.Errorf("节流期内不得请求上游，实际 %d 次", hits)
	}
}

func TestLoginPendingThenDenied(t *testing.T) {
	hits := 0
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(initResponse("flow-denied")))
			return
		}
		hits++
		if hits == 1 {
			_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"failed"}}`))
	}))
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	resetThrottle(t, statePath)
	if _, err := Poll(NewClient(), statePath); !errors.Is(err, ErrPending) {
		t.Fatalf("pending 分支：%v", err)
	}
	resetThrottle(t, statePath)
	_, err := Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "取消了授权") {
		t.Fatalf("denied 分支应给终态错误，实际 %v", err)
	}
	// 固化：后续 Poll 不得再打上游，且返回同一条错误。
	before := hits
	if _, err2 := Poll(NewClient(), statePath); err2 == nil || err2.Error() != err.Error() {
		t.Errorf("终态错误应被固化并短路，实际 %v", err2)
	}
	if hits != before {
		t.Errorf("终态短路后不得请求上游，实际多出 %d 次", hits-before)
	}
}

func TestLoginExpired(t *testing.T) {
	hits := 0
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(initResponse("flow-exp")))
			return
		}
		hits++
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"pending"}}`))
	}))
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 本地上限已过（服务端 expires_at 到期）。
	st, _ := readState(statePath)
	st.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	st.NextPollAt = 0
	if err := writeState(statePath, st); err != nil {
		t.Fatal(err)
	}
	_, err := Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("本地过期应给终态错误，实际 %v", err)
	}
	if hits != 0 {
		t.Errorf("已过期不得请求上游，实际 %d 次", hits)
	}

	// 服务端声明 expired 也算终态过期。
	st.Err = ""
	st.ExpiresAt = time.Now().Add(time.Hour).Unix()
	st.NextPollAt = 0
	if err := writeState(statePath, st); err != nil {
		t.Fatal(err)
	}
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"status":"expired"}}`))
	}))
	_, err = Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("status=expired 应给终态过期错误，实际 %v", err)
	}
}

// TestLoginPoll4xxTerminal 4xx（除 408/429）是终态，且错误文案**不得含凭据**。
func TestLoginPoll4xxTerminal(t *testing.T) {
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(initResponse("flow-403")))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		// 故意在响应体里塞一个"看起来像 token"的字段：错误文案不得把它带出去。
		_, _ = w.Write([]byte(`{"code":1002,"msg":"forbidden","access_token":"SECRET-TOKEN-SHOULD-NOT-LEAK"}`))
	}))
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	resetThrottle(t, statePath)
	_, err := Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "轮询被拒") {
		t.Fatalf("4xx 应给终态错误，实际 %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN-SHOULD-NOT-LEAK") {
		t.Errorf("零 token 纪律：错误文案不得回显响应体原文：%v", err)
	}

	// 429 是例外：属可重试（继续 pending）。
	st, _ := readState(statePath)
	st.Err = ""
	st.NextPollAt = 0
	if err := writeState(statePath, st); err != nil {
		t.Fatal(err)
	}
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	if _, err := Poll(NewClient(), statePath); !errors.Is(err, ErrPending) {
		t.Fatalf("429 应继续 pending，实际 %v", err)
	}
}

func TestLoginPoll5xxKeepsPending(t *testing.T) {
	hits := 0
	useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(initResponse("flow-500")))
			return
		}
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	resetThrottle(t, statePath)
	if _, err := Poll(NewClient(), statePath); !errors.Is(err, ErrPending) {
		t.Fatalf("5xx 应继续 pending，实际 %v", err)
	}
	if hits != 1 {
		t.Errorf("poll 请求数 = %d，期望 1", hits)
	}
	st, _ := readState(statePath)
	if st.Err != "" {
		t.Errorf("5xx 不得固化成终态：%q", st.Err)
	}
}

// TestLoginReadyFieldValidation ready 响应缺关键字段 → 终态失败（不伪造成功）。
func TestLoginReadyFieldValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 token", `{"code":0,"data":{"status":"ready","user":{"user_id":"u1"},"zai":{"access_token":"t"}}}`, "token / user_id"},
		{"缺 user_id", `{"code":0,"data":{"status":"ready","token":"j","zai":{"access_token":"t"}}}`, "token / user_id"},
		{"两个第三方凭据都空", `{"code":0,"data":{"status":"ready","token":"j","user":{"user_id":"u1"}}}`, "第三方 access_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					_, _ = w.Write([]byte(initResponse("f")))
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			if _, err := Start(NewClient(), statePath); err != nil {
				t.Fatalf("Start: %v", err)
			}
			resetThrottle(t, statePath)
			_, err := Poll(NewClient(), statePath)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应因 %q 而终态失败，实际 %v", tc.want, err)
			}
		})
	}
}

// TestLoginInitErrors ① 的失败分支。
func TestLoginInitErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"业务码非 0", 200, `{"code":1002,"msg":"unauthorized"}`, "业务码"},
		{"缺 flow_id", 200, `{"code":0,"data":{"authorize_url":"https://x/y"}}`, "flow_id"},
		{"authorize_url 非 https", 200, `{"code":0,"data":{"flow_id":"f","authorize_url":"http://x/y"}}`, "authorize_url"},
		{"HTTP 400", 400, `{"code":3001,"msg":"parameter error"}`, "授权初始化失败"},
		{"响应非 JSON", 200, `not-json`, "不是 JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			dir := t.TempDir()
			if _, err := Start(NewClient(), filepath.Join(dir, "state.json")); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// TestLoginMissingState Poll 在读不到 state 时明确报错（不静默）。
func TestLoginMissingState(t *testing.T) {
	dir := t.TempDir()
	if _, err := Poll(NewClient(), filepath.Join(dir, "nope.json")); err == nil ||
		!strings.Contains(err.Error(), "read state") {
		t.Fatalf("缺 state 应明确报错，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// SaveAuth 约束
// ---------------------------------------------------------------------------

func TestSaveAuthRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveAuth(dir, Result{ExpiresAt: time.Now().Unix()}); err == nil {
		t.Error("缺 uid 应被拒")
	}
	if _, err := SaveAuth(dir, Result{UID: "u1"}); err == nil {
		t.Error("缺 expiresAt 应被拒（不可续期渠道不能落 0 到期时间）")
	}
}

// TestNewDeviceMidUnique 设备标识必须每次生成不同的值（同一账号重复登录时也不再复用官方 mid）。
func TestNewDeviceMidUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		mid := newDeviceMid()
		if len(mid) != 36 || len(strings.Split(mid, "-")) != 5 {
			t.Fatalf("device_mid 形态不符：%q", mid)
		}
		if seen[mid] {
			t.Fatalf("device_mid 重复：%q", mid)
		}
		seen[mid] = true
	}
}

// TestSafeFileToken 账号标识进文件名前必须清洗（防路径穿越）。
func TestSafeFileToken(t *testing.T) {
	cases := map[string]string{
		"12345678901234567": "12345678901234567",
		"../../etc/passwd":  ".._.._etc_passwd",
		"a/b:c":             "a_b_c",
		"":                  "unknown",
		"..":                "unknown",
	}
	for in, want := range cases {
		if got := safeFileToken(in); got != want {
			t.Errorf("safeFileToken(%q) = %q，期望 %q", in, got, want)
		}
	}
}

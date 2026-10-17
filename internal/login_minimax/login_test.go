package login_minimax

// login_test.go 回归测试：锁定 MiniMax 设备码登录的四个易错点。
//
// 全部走**本机 httptest**（127.0.0.1 随机端口），**绝不**指向生产上游
// （AGENTS.md §0.1：严禁穷举/批量探测上游接口）。
//  1. PKCE 派生规则（32 字节随机 → base64url；challenge = sha256(verifier) 的 base64url）；
//  2. `pending` 的**双信封**都要认（200+status=pending / 非 200+error=authorization_pending）；
//  3. `expiresAt` 只能由 `now + expires_in` 得到（MiniMax token 非 JWT）；
//  4. 令牌硬校验 + 终态错误固化（不再重复打上游）+ SaveAuth/LoadMinimaxDir 往返。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// withTestHost 把包内 endpointHost 指向本机测试服务，测试结束恢复。
func withTestHost(t *testing.T, host string) {
	t.Helper()
	old := endpointHost
	endpointHost = host
	t.Cleanup(func() { endpointHost = old })
}

// clearThrottle 把 NextPollAt 归零，让下一次 Poll 立即发请求（避免测试里 sleep）。
func clearThrottle(t *testing.T, statePath string) {
	t.Helper()
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	st.NextPollAt = time.Now().Add(-time.Second).Unix()
	if err := writeState(statePath, st); err != nil {
		t.Fatalf("write state: %v", err)
	}
}

// TestStartPKCEAndDeviceCode 校验申请设备码的请求体、PKCE 派生、以及
// 「verification_uri_complete 缺失时回退 verification_uri」。
func TestStartPKCEAndDeviceCode(t *testing.T) {
	var gotForm url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		_, _ = w.Write([]byte(`{"device_code":"dc-1","user_code":"UC-1",
			"verification_uri":"https://account.minimax.cn/device","expires_in":600,"interval":7}`))
	}))
	defer srv.Close()
	withTestHost(t, srv.URL)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	open, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if gotPath != deviceCodePath {
		t.Fatalf("路径 = %q, want %q", gotPath, deviceCodePath)
	}
	for k, want := range map[string]string{
		"client_id":             ClientID,
		"scope":                 Scope,
		"audience":              Audience,
		"code_challenge_method": "S256",
	} {
		if got := gotForm.Get(k); got != want {
			t.Errorf("设备码请求体 %s = %q, want %q", k, got, want)
		}
	}
	// verification_uri_complete 缺失 ⇒ 回退 verification_uri
	if open != "https://account.minimax.cn/device" {
		t.Fatalf("返回链接 = %q, want 回退到 verification_uri", open)
	}
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.DeviceCode != "dc-1" || st.CodeVerifier == "" || st.IntervalSec != 7 {
		t.Fatalf("state 落盘不符: %+v", st)
	}
	// PKCE：challenge = base64url(sha256(verifier))
	sum := sha256.Sum256([]byte(st.CodeVerifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); gotForm.Get("code_challenge") != want {
		t.Fatalf("code_challenge = %q, want %q", gotForm.Get("code_challenge"), want)
	}
	// verifier = base64url(32 字节) ⇒ 解码后必须正好 32 字节
	rawVerifier, err := base64.RawURLEncoding.DecodeString(st.CodeVerifier)
	if err != nil || len(rawVerifier) != 32 {
		t.Fatalf("code_verifier 不是 32 字节 base64url: len=%d err=%v", len(rawVerifier), err)
	}
	if st.ExpiresAt < time.Now().Add(9*time.Minute).Unix() {
		t.Fatalf("expiresAt 未按 expires_in 写入: %d", st.ExpiresAt)
	}
}

// TestPollPendingBothEnvelopes 是本包最关键的回归：pending 有两种信封，
// 少认一种就会把"还没授权"误判成失败或误判成成功。
func TestPollPendingBothEnvelopes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == deviceCodePath {
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"UC","verification_uri":"https://x/y",
				"verification_uri_complete":"https://x/y?user_code=UC","expires_in":600,"interval":5}`))
			return
		}
		calls++
		switch calls {
		case 1: // 形态①：HTTP 200 + status=pending
			_, _ = w.Write([]byte(`{"status":"pending"}`))
		case 2: // 形态②：非 200 + error=authorization_pending
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending","error_description":"wait"}`))
		case 3: // slow_down（200 形态）
			_, _ = w.Write([]byte(`{"status":"slow_down"}`))
		default:
			_, _ = w.Write([]byte(`{"access_token":"mmoat_abc","refresh_token":"mmort_def",
				"token_type":"bearer","expires_in":3600,"scope":"agent.default other"}`))
		}
	}))
	defer srv.Close()
	withTestHost(t, srv.URL)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 四次轮询各代表一种上游形态，每次前清掉本地节流
	for i, wantPending := range []bool{true, true, true, false} {
		clearThrottle(t, statePath)
		res, err := Poll(NewClient(), statePath)
		if wantPending {
			if !errors.Is(err, ErrPending) {
				t.Fatalf("第 %d 次轮询应 pending，got res=%+v err=%v", i+1, res, err)
			}
			st, _ := readState(statePath)
			if st.Err != "" {
				t.Fatalf("pending 被误判为终态错误: %q", st.Err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("第 %d 次轮询应成功: %v", i+1, err)
		}
		if res.AccessToken != "mmoat_abc" || res.RefreshToken != "mmort_def" {
			t.Fatalf("令牌字段解析错误: %+v", res)
		}
		sum := sha256.Sum256([]byte("mmoat_abc"))
		wantUID := hex.EncodeToString(sum[:])[:16]
		if res.UID != wantUID {
			t.Fatalf("UID = %q, want %q（access_token 派生）", res.UID, wantUID)
		}
		// 非 JWT：expiresAt 必须 ≈ now + expires_in
		if d := res.ExpiresAt - time.Now().Unix(); d < 3595 || d > 3600 {
			t.Fatalf("expiresAt 与 now+expires_in 不符: delta=%d", d)
		}
		if _, err := os.Stat(statePath); !os.IsNotExist(err) {
			t.Fatal("登录成功后 state 文件应被删除")
		}
	}
	// slow_down 必须把 interval +5s（第 3 次调用后立刻检查下一个 NextPollAt 的间隔）
	// 说明：第 3 次轮询即 slow_down，已在上面循环里走完；这里用最终成功路径无法回看，
	// 故单独用一次 slow_down 状态检查，见 TestPollSlowDownAndTerminalErrors。
	if calls != 4 {
		t.Fatalf("上游 token 端点被调用 %d 次，want 4", calls)
	}
}

// TestPollSlowDownAndTerminalErrors 覆盖 slow_down 的退避步长与三类终态错误
// （拒绝 / 过期 / 其他非 2xx），并验证终态错误会固化、不再重复打上游。
func TestPollSlowDownAndTerminalErrors(t *testing.T) {
	mode := "slow_down"
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == deviceCodePath {
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"UC","verification_uri":"https://x/y",
				"expires_in":600,"interval":6}`))
			return
		}
		calls++
		switch mode {
		case "slow_down":
			_, _ = w.Write([]byte(`{"status":"slow_down"}`))
		case "denied":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"access_denied"}`))
		case "expired":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"expired_token"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"server_error","error_description":"boom"}`))
		}
	}))
	defer srv.Close()
	withTestHost(t, srv.URL)

	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	if _, err := Start(NewClient(), statePath); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// slow_down → interval 6+5=11
	clearThrottle(t, statePath)
	if _, err := Poll(NewClient(), statePath); !errors.Is(err, ErrPending) {
		t.Fatalf("slow_down 应返回 ErrPending, got %v", err)
	}
	st, _ := readState(statePath)
	if st.IntervalSec != 11 {
		t.Fatalf("slow_down 后 interval = %d, want 11", st.IntervalSec)
	}
	if st.NextPollAt < time.Now().Add(9*time.Second).Unix() {
		t.Fatalf("slow_down 未按新 interval 退避: nextPollAt=%d", st.NextPollAt)
	}
	if st.Err != "" {
		t.Fatalf("slow_down 被误判为终态错误: %q", st.Err)
	}

	for _, tc := range []struct{ mode, wantSub string }{
		{"denied", "用户取消"},
		{"expired", "过期"},
		{"other", "授权失败"},
	} {
		mode = tc.mode
		clearThrottle(t, statePath)
		before := calls
		_, err := Poll(NewClient(), statePath)
		if err == nil {
			t.Fatalf("%s 应报错", tc.mode)
		}
		if !contains(err.Error(), tc.wantSub) {
			t.Fatalf("%s 错误文案 = %q, want 含 %q", tc.mode, err.Error(), tc.wantSub)
		}
		if calls != before+1 {
			t.Fatalf("%s 应恰好打 1 次上游，got %d", tc.mode, calls-before)
		}
		// 终态固化：再轮询不得再打上游
		if _, err := Poll(NewClient(), statePath); err == nil {
			t.Fatalf("%s 二次轮询应继续报错", tc.mode)
		}
		if calls != before+1 {
			t.Fatalf("%s 终态错误未固化，二次轮询又打了上游（calls=%d）", tc.mode, calls)
		}
		// 清掉固化错误，进入下一个终态用例
		st, _ := readState(statePath)
		st.Err = ""
		_ = writeState(statePath, st)
	}
}

// TestPollTokenHardValidation 覆盖 §3.5 的五条硬校验（任一条不满足即抛）。
func TestPollTokenHardValidation(t *testing.T) {
	body := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == deviceCodePath {
			_, _ = w.Write([]byte(`{"device_code":"dc","user_code":"UC","verification_uri":"https://x/y",
				"expires_in":600,"interval":1}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	withTestHost(t, srv.URL)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 access_token", `{"refresh_token":"r","token_type":"bearer","expires_in":60,"scope":"agent.default"}`, "access_token"},
		{"缺 refresh_token", `{"access_token":"a","token_type":"bearer","expires_in":60,"scope":"agent.default"}`, "refresh_token"},
		{"token_type 不是 bearer", `{"access_token":"a","refresh_token":"r","token_type":"mac","expires_in":60,"scope":"agent.default"}`, "token_type"},
		{"expires_in 非正数", `{"access_token":"a","refresh_token":"r","token_type":"bearer","expires_in":0,"scope":"agent.default"}`, "expires_in"},
		{"scope 不含 agent.default", `{"access_token":"a","refresh_token":"r","token_type":"bearer","expires_in":60,"scope":"other.scope"}`, "scope"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		statePath := filepath.Join(dir, "login-state.json")
		if _, err := Start(NewClient(), statePath); err != nil {
			t.Fatalf("%s: Start: %v", tc.name, err)
		}
		body = tc.body
		clearThrottle(t, statePath)
		if _, err := Poll(NewClient(), statePath); err == nil || !contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want 含 %q", tc.name, err, tc.want)
		}
	}
}

// TestSaveAuthRoundTripAndGlobBoundary 校验落盘与加载器往返，以及 glob 前缀边界
// （minimax2-*.json 不得被 LoadMinimaxDir 吞进来）。
func TestSaveAuthRoundTripAndGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	r := Result{
		AccessToken:  "mmoat_x",
		RefreshToken: "mmort_y",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Domain:       Domain,
		ApiHost:      ApiHost,
		UID:          "0123456789abcdef",
	}
	fp, err := SaveAuth(dir, r)
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	if filepath.Base(fp) != "minimax-0123456789abcdef.json" {
		t.Fatalf("文件名 = %q", filepath.Base(fp))
	}
	// 借道另一渠道前缀：内容合法但不应被本加载器收
	other := filepath.Join(dir, "minimax2-other.json")
	raw, _ := os.ReadFile(fp)
	if err := os.WriteFile(other, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	auths, err := auth.LoadMinimaxDir(dir)
	if err != nil {
		t.Fatalf("LoadMinimaxDir: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("LoadMinimaxDir 收到 %d 个账号，want 1（minimax2- 不得被吞）", len(auths))
	}
	a := auths[0]
	if a.Kind != "minimax" || a.UID != r.UID || a.AccessToken != r.AccessToken || a.RefreshToken != r.RefreshToken {
		t.Fatalf("往返后字段不符: %+v", a)
	}
	if a.ExpiresAt != r.ExpiresAt || a.FilePath != fp || a.Nickname != r.UID {
		t.Fatalf("往返后 expiresAt/FilePath/nickname 不符: %+v", a)
	}
	// expiresAt 落 0 必须报错（非 JWT 渠道没有事后校正机会）
	if _, err := SaveAuth(dir, Result{UID: "u", AccessToken: "a", RefreshToken: "r"}); err == nil {
		t.Fatal("expiresAt=0 应被拒绝")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

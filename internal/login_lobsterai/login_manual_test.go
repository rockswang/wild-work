// login_manual_test.go 手动回调（局域网/远程兜底）的**契约测试**：**零真实网络**
// （exchange 由本机 httptest 替身承接，绝不打生产上游，AGENTS.md §0.1）。
//
// 注意用词：这不是「回归测试」——回归测试必须修复前失败、修复后通过；而本文件测的是
// 新增的 SubmitCallbackURL 入口，在未修复的 v2.9.1 上连编译都过不去
// （undefined: SubmitCallbackURL）。它的作用是锁定契约：
//  1. 完整 URL（端口一致 + state 正确）能走通与自动回调**相同**的兑换路径；
//  2. 端口自检拦下「上一次登录」的地址，且**不污染**当前会话；
//  3. state 仍是唯一防伪手段——裸 code / state 不符一律拒绝（手动通道不得放宽它）；
//  4. 缺 code、用户拒绝、无进行中流程都要给出可行动的错误。
package login_lobsterai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExchange 起 exchange 替身（记录收到的 authCode），并把包内 endpointHost 指向它。
func fakeExchange(t *testing.T) (gotCode *string) {
	t.Helper()
	code := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		code = body["authCode"]
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"accessToken":"at-m","refreshToken":"rt-m","expiresIn":7200,"user":{"id":12345,"nickname":"王**"}}}`))
	}))
	t.Cleanup(srv.Close)
	withTestEndpoint(t, srv.URL, "2026.9.9")
	return &code
}

// callbackURL 拼一条「本次登录」的回调地址（端口取自本次登录的回调口）。
func callbackURL(st state, code string, withState bool) string {
	raw := "http://127.0.0.1:" + itoa(st.CallbackPort) + callbackPath +
		"?return_to=" + url.QueryEscape("https://lobsterai.youdao.com/portal#/login") + "&code=" + url.QueryEscape(code)
	if withState {
		raw += "&state=" + url.QueryEscape(st.State)
	}
	return raw
}

// TestSubmitCallbackURLFullURL 完整回调 URL（端口与本次登录一致、state 正确）应被接受。
func TestSubmitCallbackURLFullURL(t *testing.T) {
	gotCode := fakeExchange(t)
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	st := startTestLogin(t, statePath)

	if err := SubmitCallbackURL(callbackURL(st, "C-URL", true)); err != nil {
		t.Fatalf("SubmitCallbackURL(完整 URL): %v", err)
	}
	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if *gotCode != "C-URL" {
		t.Errorf("exchange 收到的 authCode = %q，期望 C-URL", *gotCode)
	}
	if res.UID != "12345" {
		t.Errorf("UID = %q，期望 12345（走真实 exchange 解析）", res.UID)
	}
}

// TestSubmitCallbackURLPortMismatch 粘贴上一次登录的回调地址时立刻报错，
// 且**不得污染**当前会话（Poll 仍应 Pending）。
func TestSubmitCallbackURLPortMismatch(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	st := startTestLogin(t, statePath)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort+1) + callbackPath +
		"?code=c-old&state=" + url.QueryEscape(st.State)
	err := SubmitCallbackURL(raw)
	if err == nil {
		t.Fatal("端口不一致应报错，实际 nil")
	}
	if !strings.Contains(err.Error(), "不一致") {
		t.Errorf("错误信息应说明端口不一致，得到: %v", err)
	}
	if _, perr := Poll(NewClient(), statePath); perr != ErrPending {
		t.Errorf("端口不符不应写入会话，Poll 期望 ErrPending，得到 %v", perr)
	}
}

// TestSubmitCallbackURLRequiresState 裸授权码没有 state：本渠道不得放宽防伪，
// 必须拒绝并让 Poll 进入终态失败（不允许把无 state 的 code 换出去）。
func TestSubmitCallbackURLRequiresState(t *testing.T) {
	gotCode := fakeExchange(t)
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	startTestLogin(t, statePath)

	err := SubmitCallbackURL("c-bare")
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("裸 code 应以缺 state 拒绝，得到 %v", err)
	}
	if *gotCode != "" {
		t.Errorf("被拒的回调不得触发 exchange，实际收到 code=%q", *gotCode)
	}
	if _, perr := Poll(NewClient(), statePath); perr == nil || perr == ErrPending {
		t.Errorf("缺 state 应让 Poll 终态失败，得到 %v", perr)
	}
}

// TestSubmitCallbackURLStateMismatch state 不符（别条流程的地址）：拒绝并报出原因。
func TestSubmitCallbackURLStateMismatch(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	st := startTestLogin(t, statePath)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort) + callbackPath + "?code=c-x&state=wrong-state"
	err := SubmitCallbackURL(raw)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("state 不符应报错，得到 %v", err)
	}
	if _, perr := Poll(NewClient(), statePath); perr == nil || perr == ErrPending {
		t.Errorf("state 不符应让 Poll 终态失败，得到 %v", perr)
	}
}

// TestSubmitCallbackURLMissingCode 缺 code 参数（只粘了 callback 路径）：报错并终态失败。
func TestSubmitCallbackURLMissingCode(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	st := startTestLogin(t, statePath)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort) + callbackPath + "?state=" + url.QueryEscape(st.State)
	err := SubmitCallbackURL(raw)
	if err == nil || !strings.Contains(err.Error(), "code") {
		t.Fatalf("缺 code 应报错，得到 %v", err)
	}
	if _, perr := Poll(NewClient(), statePath); perr == nil || perr == ErrPending {
		t.Errorf("缺 code 应让 Poll 终态失败，得到 %v", perr)
	}
}

// TestSubmitCallbackURLUserDenied 用户在登录页拒绝：错误文案透传 error_description。
func TestSubmitCallbackURLUserDenied(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "login-state.json")
	st := startTestLogin(t, statePath)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort) + callbackPath +
		"?error=access_denied&error_description=user+denied&state=" + url.QueryEscape(st.State)
	err := SubmitCallbackURL(raw)
	if err == nil || !strings.Contains(err.Error(), "user denied") {
		t.Fatalf("应透传 error_description，得到 %v", err)
	}
}

// TestSubmitCallbackURLNoSession 没有进行中的登录流程时给出明确提示（而非静默丢弃）。
func TestSubmitCallbackURLNoSession(t *testing.T) {
	startTestLogin(t, filepath.Join(t.TempDir(), "login-state.json"))
	Shutdown()

	err := SubmitCallbackURL("http://127.0.0.1:41531/auth/callback?code=c1&state=s1")
	if err == nil || !strings.Contains(err.Error(), "没有进行中的登录流程") {
		t.Fatalf("无会话应明确报错，得到 %v", err)
	}
}

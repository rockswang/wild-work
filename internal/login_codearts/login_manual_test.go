// login_manual_test.go 手动回调（局域网/远程兜底）的**契约测试**：**零真实网络**。
//
// 注意用词：这不是「回归测试」。回归测试必须能修复前失败、修复后通过；而本文件测的是
// 本 PR 新增的 SubmitCallbackURL 入口，在未修复的 v2.9.0 上连编译都过不去
// （实测 undefined: SubmitCallbackURL），不可能呈现红→绿。
// 它的作用是锁定契约：端口自检、缺 code/拒绝的语义、人工通道与自动回调共用同一结论通道。
package login_codearts

import (
	"strings"
	"testing"

	"wild-work/internal/codearts"
)

// TestSubmitCallbackURLBareCode 只贴裸授权码也能登录（Poll 走同一兑换路径）。
func TestSubmitCallbackURLBareCode(t *testing.T) {
	sts := newFakeSTS(t)
	old := stsURL
	stsURL = sts.URL
	defer func() { stsURL = old }()

	_, _, statePath := startLogin(t)

	if err := SubmitCallbackURL("c-bare"); err != nil {
		t.Fatalf("SubmitCallbackURL(裸 code): %v", err)
	}
	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.AccessKeyID != "AK-NEW" {
		t.Errorf("AccessKeyID = %q，期望 AK-NEW", res.AccessKeyID)
	}
	if got := sts.form.Get("code"); got != "c-bare" {
		t.Errorf("STS 收到的 code = %q，期望 c-bare", got)
	}
}

// TestSubmitCallbackURLFullURL 完整回调 URL（端口与本次登录一致）应被接受。
func TestSubmitCallbackURLFullURL(t *testing.T) {
	sts := newFakeSTS(t)
	old := stsURL
	stsURL = sts.URL
	defer func() { stsURL = old }()

	st, _, statePath := startLogin(t)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath + "?code=c-url&state=x"
	if err := SubmitCallbackURL(raw); err != nil {
		t.Fatalf("SubmitCallbackURL(完整 URL): %v", err)
	}
	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.RefreshToken != "rt-new" {
		t.Errorf("RefreshToken = %q，期望 rt-new", res.RefreshToken)
	}
	if got := sts.form.Get("redirect_uri"); got != st.RedirectURI {
		t.Errorf("redirect_uri = %q，期望 %q（必须与 Start 落盘的一致）", got, st.RedirectURI)
	}
}

// TestSubmitCallbackURLPortMismatch 粘贴上一次登录的回调地址时立刻报错，
// 且**不得污染**当前会话（Poll 仍应 Pending）。
func TestSubmitCallbackURLPortMismatch(t *testing.T) {
	st, _, statePath := startLogin(t)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort+1) + codearts.OAuthCallbackPath + "?code=c-old"
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

// TestSubmitCallbackURLMissingCode 缺 code 参数：报错并让 Poll 进入终态失败。
func TestSubmitCallbackURLMissingCode(t *testing.T) {
	st, _, statePath := startLogin(t)

	raw := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath
	err := SubmitCallbackURL(raw)
	if err == nil || !strings.Contains(err.Error(), "code") {
		t.Fatalf("缺 code 应报错，得到 %v", err)
	}
	if _, perr := Poll(NewClient(), statePath); perr == nil || perr == ErrPending {
		t.Errorf("缺 code 应让 Poll 终态失败，得到 %v", perr)
	}
}

// TestSubmitCallbackURLUserDenied 用户在授权页拒绝：错误文案透传（含 error_description）。
func TestSubmitCallbackURLUserDenied(t *testing.T) {
	startLogin(t)

	if err := SubmitCallbackURL("?error=access_denied&error_description=user+denied"); err == nil {
		t.Fatal("用户拒绝应报错，实际 nil")
	} else if !strings.Contains(err.Error(), "user denied") {
		t.Errorf("应透传 error_description，得到: %v", err)
	}
}

// TestSubmitCallbackURLNoSession 没有进行中的登录流程时给出明确提示（而非静默丢弃）。
func TestSubmitCallbackURLNoSession(t *testing.T) {
	startLogin(t)
	Shutdown()

	err := SubmitCallbackURL("http://127.0.0.1:41531/oauth/callback?code=c1")
	if err == nil || !strings.Contains(err.Error(), "没有进行中的登录流程") {
		t.Fatalf("无会话应明确报错，得到 %v", err)
	}
}

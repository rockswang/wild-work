package login_trae

// login_manual_test.go 锁死「粘贴回调地址」人工兜底通道的契约（issue #98）。
//
// 背景：TraeWork 的回调写死 `http://127.0.0.1:<随机端口>/authorize` 且监听只有 5 分钟
// 寿命，浏览器与网关不同机（局域网/远程面板）或超时时凭证到不了网关。SubmitCallbackURL
// 让用户把地址栏 URL 粘回来，写入 pending 的 state 文件——与真实回调同一条结论通道。
//
// 全部离线：只操作 state 文件，不发起任何上游请求（authCode/refreshToken 的兑换由
// Poll 路径完成，不在本测试范围；凭证值均为假 token，遵守零真 token 纪律）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/traework"
)

func writeTestState(t *testing.T, st state) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "login-state.json")
	if err := writeState(p, st); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	return p
}

func readTestState(t *testing.T, p string) state {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	return st
}

// TestSubmitCallbackURLFullURL 完整回调 URL（端口与本次登录一致）应被接受，
// 凭证写入 state 文件（Poll 下一跳即兑换），且会话原有字段不被破坏。
func TestSubmitCallbackURLFullURL(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CodeVerifier: "v1", CallbackPort: 18080})
	raw := "http://127.0.0.1:18080/authorize?isRedirect=true&scope=solo&refreshToken=rt-1&userInfo=%7B%22id%22%3A1%7D"
	if err := SubmitCallbackURL(p, raw); err != nil {
		t.Fatalf("SubmitCallbackURL(完整 URL): %v", err)
	}
	st := readTestState(t, p)
	if st.RefreshToken != "rt-1" {
		t.Fatalf("RefreshToken = %q, 期望 rt-1", st.RefreshToken)
	}
	if st.Host != traework.OAuthHost {
		t.Fatalf("Host = %q, 期望默认 %q（无 host 参数时兜底）", st.Host, traework.OAuthHost)
	}
	if st.Err != "" {
		t.Fatalf("成功提交后 Err 应为空，实际 %q", st.Err)
	}
	if st.CodeVerifier != "v1" || st.MachineID != "m1" || st.DeviceID != "d1" {
		t.Fatalf("会话原有字段不得被破坏: %+v", st)
	}
}

// TestSubmitCallbackURLAuthCode PKCE 新流程：authCodeInfo JSON 形态 + host 参数覆盖默认网关。
func TestSubmitCallbackURLAuthCode(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CodeVerifier: "v1", CallbackPort: 18081})
	raw := "http://127.0.0.1:18081/authorize?authCodeInfo=%7B%22AuthCode%22%3A%22ac-1%22%7D&host=api.trae.example"
	if err := SubmitCallbackURL(p, raw); err != nil {
		t.Fatalf("SubmitCallbackURL(authCodeInfo): %v", err)
	}
	st := readTestState(t, p)
	if st.AuthCode != "ac-1" {
		t.Fatalf("AuthCode = %q, 期望 ac-1", st.AuthCode)
	}
	if st.Host != "api.trae.example" {
		t.Fatalf("Host = %q, 期望随回调携带的 host 参数", st.Host)
	}
}

// TestSubmitCallbackURLUserJwt 旧形态：userJwt（URL 编码 JSON）兜底取 RefreshToken。
func TestSubmitCallbackURLUserJwt(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CallbackPort: 18082})
	raw := `http://127.0.0.1:18082/authorize?userJwt=%7B%22RefreshToken%22%3A%22rt-jwt%22%7D`
	if err := SubmitCallbackURL(p, raw); err != nil {
		t.Fatalf("SubmitCallbackURL(userJwt): %v", err)
	}
	st := readTestState(t, p)
	if st.RefreshToken != "rt-jwt" {
		t.Fatalf("RefreshToken = %q, 期望 rt-jwt（userJwt 兜底）", st.RefreshToken)
	}
}

// TestSubmitCallbackURLPortMismatch 粘贴上一次登录的回调地址时立刻报错，
// state 不得被写入任何凭证。
func TestSubmitCallbackURLPortMismatch(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CallbackPort: 18083})
	raw := "http://127.0.0.1:9999/authorize?refreshToken=rt-stale"
	err := SubmitCallbackURL(p, raw)
	if err == nil {
		t.Fatal("端口不符应报错")
	}
	if !strings.Contains(err.Error(), "端口") {
		t.Fatalf("报错应说明端口不一致，实际: %v", err)
	}
	if st := readTestState(t, p); st.RefreshToken != "" {
		t.Fatalf("被拒的提交不得写入凭证，实际 RefreshToken=%q", st.RefreshToken)
	}
}

// TestSubmitCallbackURLNoSession 没有进行中的登录流程时给出明确提示（而非静默丢弃）。
func TestSubmitCallbackURLNoSession(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent.json")
	err := SubmitCallbackURL(p, "http://127.0.0.1:18084/authorize?refreshToken=rt-x")
	if err == nil || !strings.Contains(err.Error(), "没有进行中") {
		t.Fatalf("无会话应明确报错，实际: %v", err)
	}
}

// TestSubmitCallbackURLMissingCredentials 缺凭证参数：报错并把原因固化到 state.Err，
// 让下一次轮询拿到可行动的终态（与自动回调的失败态一致）。
func TestSubmitCallbackURLMissingCredentials(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CallbackPort: 18085})
	raw := "http://127.0.0.1:18085/authorize?scope=solo&isRedirect=true"
	if err := SubmitCallbackURL(p, raw); err == nil {
		t.Fatal("缺凭证应报错")
	}
	st := readTestState(t, p)
	if st.Err == "" {
		t.Fatal("失败原因应固化到 state.Err")
	}
	if st.RefreshToken != "" || st.AuthCode != "" {
		t.Fatalf("缺凭证时不得写入任何凭证: %+v", st)
	}
}

// TestSubmitCallbackURLAlreadyReceived 已收到凭证的会话拒绝二次提交（保留先到者，
// 与自动回调「不覆盖」一致）。
func TestSubmitCallbackURLAlreadyReceived(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CallbackPort: 18086, RefreshToken: "first"})
	err := SubmitCallbackURL(p, "http://127.0.0.1:18086/authorize?refreshToken=second")
	if err == nil {
		t.Fatal("已有凭证时应拒绝二次提交")
	}
	if st := readTestState(t, p); st.RefreshToken != "first" {
		t.Fatalf("先到的凭证不得被覆盖，实际 %q", st.RefreshToken)
	}
}

// TestSubmitCallbackURLBareToken 只贴单个参数值应明确报错（无法判断字段含义）。
func TestSubmitCallbackURLBareToken(t *testing.T) {
	p := writeTestState(t, state{MachineID: "m1", DeviceID: "d1", CallbackPort: 18087})
	if err := SubmitCallbackURL(p, "rt-bare"); err == nil {
		t.Fatal("裸值应报错")
	}
}

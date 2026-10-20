package app

// login_manual_callback_test.go 锁死「人工回调通道」的白名单与单一来源。
//
// 背景（R96 / issue #160 的同类面）：授权回调写死回环地址的渠道，浏览器与网关不同机时
// 授权码到不了网关，必须靠用户把地址栏 URL 粘回来。这条链路跨两处，**漏改一处不报错**：
//
//	GET  /api/login/start     → manual_callback 字段（前端据此显隐输入行）
//	POST /api/login/callback  → 接受白名单（决定提交是否被 400）
//
// 若 start 说 true 而 callback 的 switch 没有对应分支，提交会「成功但什么都不做」
// （校验通过、无分派、直接回 ok），用户看到「已提交，正在换取凭据…」后一直等到超时；
// 若 start 说 false 而 callback 支持，则前端根本没有入口。故两处都必须走 manualCallbackKind。

import (
	"os"
	"strings"
	"testing"

	logingemini "wild-work/internal/login_gemini"
	loginqwenwork "wild-work/internal/login_qwenwork"
	"wild-work/internal/provider"
)

// TestManualCallbackKindWhitelist 需要人工通道的渠道必须为 true，其余必须为 false。
func TestManualCallbackKindWhitelist(t *testing.T) {
	want := map[provider.Kind]bool{
		provider.CodeArts:   true, // portal 只接受 port 参数，跳转目标写死回环
		provider.LobsterAI:  true, // redirect_uri 回环 + 回调口绝不监听非环回
		provider.TraeWork:   true, // 回调写死 127.0.0.1 随机端口 + 5 分钟监听寿命（issue #98）
		provider.QwenWork:   true, // PKCE(Hydra) 回环回调，redirect_uri 白名单制不可改
		provider.Gemini:     true, // installed-app 政策限 redirect host 为 localhost，不可改
		provider.GLM:        false, // 网关侧 CDP 捕获
		provider.Raccoon:    false, // 网关侧 CDP 捕获
		provider.OfficeAce:  false, // 云端轮询捕获
		provider.Loomy:      false, // 本机登录引导页（用户直接在上面填手机号验证码）
	}
	for kind, exp := range want {
		if got := manualCallbackKind(kind); got != exp {
			t.Errorf("manualCallbackKind(%s) = %v，期望 %v（前端显隐/后端白名单都依赖它）", kind, got, exp)
		}
	}
}

// TestGeminiQwenWorkPasteEndpoint 粘贴白名单接线验证：qwenwork/gemini 纳入后
// 不得出现「白名单 true 但 handler 不认」的半接线（真实 code 兑换链路已在各包测试覆盖）。
func TestGeminiQwenWorkPasteEndpoint(t *testing.T) {
	for _, kind := range []provider.Kind{provider.QwenWork, provider.Gemini} {
		if !manualCallbackKind(kind) {
			t.Fatalf("%s 应在粘贴白名单内", kind)
		}
	}
	// 白名单内但无登录流程：直接走 SubmitCallbackURL 语义验证（不起 HTTP server——
	// HandleAPI 的 guardMux 依赖完整 App 装配（cfg/session），空 App 会 panic）。
	// qwenwork 的 pending 是包级内存，无流程时提交合法 code 也会被收下（与真实
	// 「回调先到」一致）；这里验证 error 形态能正确传导即可。
	if err := loginqwenwork.SubmitCallbackURL("http://127.0.0.1:8080/callback?error=access_denied"); err == nil {
		t.Fatal("qwenwork error 回调应报错")
	}
	if err := logingemini.SubmitCallbackURL("http://localhost:1/oauth-callback?code=x&state=y"); err == nil {
		t.Fatal("gemini 无会话时应报「没有进行中的登录流程」")
	} else if strings.Contains(err.Error(), "不需要手动回调") {
		t.Fatal("gemini 错误文案异常（半接线表现）")
	}
}

// TestManualCallbackSingleSource 两处 handler 都必须调用 manualCallbackKind，
// 不得各自内联 `kind == provider.X` 条件（那正是漏改一处的来源）。
func TestManualCallbackSingleSource(t *testing.T) {
	raw, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败（签名变化会让本测试静默失效）：%v", err)
	}
	src := string(raw)
	// 定义 1 处 + 调用 2 处。
	if n := strings.Count(src, "manualCallbackKind("); n < 3 {
		t.Errorf("manualCallbackKind 只出现 %d 次，期望 ≥3（定义 1 + start/callback 各 1）", n)
	}
	for _, bad := range []string{
		"kind == provider.CodeArts || kind == provider.LobsterAI",
		"kind == provider.LobsterAI || kind == provider.CodeArts",
	} {
		if strings.Contains(src, bad) {
			t.Errorf("发现内联白名单 %q —— 必须改走 manualCallbackKind，否则两处会漂移", bad)
		}
	}
}

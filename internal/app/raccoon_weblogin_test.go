package app

import (
	"os"
	"strings"
	"testing"
)

// 本测试锁定 R44：小浣熊登录必须走「网页版 CDP 捕获」，
// 且**不得**再回到已废弃的桌面协议回调路径。
//
// 背景：原实现靠改写 HKCU 的 office-raccoon 协议注册接住深链授权码，
// 仅 Windows 可用、要求本机装客户端。2026-10-10 改为网页版流程（internal/login_raccoon/web.go）。
// 这类「接线」疏漏在源码层就能判定（同 R34「声明了却没接线」、R41 回调未接线家族），
// 故用静态断言锁死，避免将来回退。
func TestRaccoonLoginUsesWebFlow(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	code := string(src)

	// ① 必须调用网页版入口（无参数签名）。
	if !strings.Contains(code, "loginraccoon.Start()") {
		t.Errorf("❌ app.go 未调用 loginraccoon.Start() —— 小浣熊网页版登录未接线")
	}
	if !strings.Contains(code, "loginraccoon.Poll()") {
		t.Errorf("❌ app.go 未调用 loginraccoon.Poll() —— 登录结果无法回传")
	}
	if !strings.Contains(code, "loginraccoon.Shutdown()") {
		t.Errorf("❌ app.go 未调用 loginraccoon.Shutdown() —— 浏览器不会被关闭")
	}

	// ② 不得再调用旧的协议回调签名（Start/Poll/Shutdown 带 statePath/stateDir/exePath）。
	//    Shutdown 的旧签名带两个参数，这里直接扫特征串。
	for _, stale := range []string{
		"loginraccoon.Shutdown(a.loginStateFP",
		"loginraccoon.Poll(a.loginClient",
		"loginraccoon.Start(a.loginClient",
		"loginraccoon.NewClient()",
	} {
		if strings.Contains(code, stale) {
			t.Errorf("❌ app.go 仍引用已废弃的协议回调调用 %q —— R44 已改用网页版流程", stale)
		}
	}
}

// TestRaccoonWebLoginConstants 网页版登录的关键常量必须与实盘抓包一致
// （2026-10-10 SAZ：Cookie 名 raccoon_refresh_token、域 .xiaohuanxiong.com）。
func TestRaccoonWebLoginConstants(t *testing.T) {
	src, err := os.ReadFile("../login_raccoon/web.go")
	if err != nil {
		t.Fatalf("读 web.go 失败: %v", err)
	}
	code := string(src)
	for _, want := range []string{
		`CookieName = "raccoon_refresh_token"`,
		`CookieDomain = "xiaohuanxiong.com"`,
		`ownerTypeUsers = "users"`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("❌ web.go 缺少关键常量 %q", want)
		}
	}
}

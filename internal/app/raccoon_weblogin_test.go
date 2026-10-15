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

// TestRaccoonUIDExtractionSingleSourceOfTruth 小浣熊 uid 提取必须**只有一处实现**
// （raccoon.JWTUID）。
//
// 背景（2026-10-15，PR #124 合并期发现）：上游迁站后 JWT 换成标准 OIDC，无 `name`
// claim（账号标识在 `sub`）。PR #124 只给「登录落盘」与「web.go」两处补了 sub 回落，
// 而 `internal/raccoon/clientsession.go`（客户端会话自愈）与 `internal/app/import_local.go`
// （从客户端导入）仍只读 name ⇒ 两路都恒空：前者报「uid 不匹配」使自愈永久失效
// （不变式 19），后者把账号落成 `raccoon-default.json`。
//
// 四处各写一份解析就是根因，故用静态断言锁住「不得再出现自行解析 JWT claim 取 uid」。
func TestRaccoonUIDExtractionSingleSourceOfTruth(t *testing.T) {
	// ① raccoon 包必须导出唯一的 JWTUID。
	src, err := os.ReadFile("../raccoon/clientsession.go")
	if err != nil {
		t.Fatalf("读 clientsession.go 失败: %v", err)
	}
	if !strings.Contains(string(src), "func JWTUID(") {
		t.Error("❌ internal/raccoon 应导出 JWTUID 作为 uid 提取唯一实现")
	}

	// ② 登录落盘与导入路径都必须委托给它，不得自行读 name/sub claim。
	for _, f := range []string{"app.go", "import_local.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", f, err)
		}
		code := string(b)
		// 去行注释后再扫，避免注释里提到旧写法（教训留档）被误判。
		var live []string
		for _, ln := range strings.Split(code, "\n") {
			if i := strings.Index(ln, "//"); i >= 0 {
				ln = ln[:i]
			}
			live = append(live, ln)
		}
		src := strings.Join(live, "\n")
		if !strings.Contains(src, "raccoon.JWTUID(") {
			t.Errorf("❌ %s 未调用 raccoon.JWTUID 提取小浣熊 uid（会漏新版 OIDC 的 sub）", f)
		}
		if strings.Contains(src, "jwtClaim(") {
			t.Errorf("❌ %s 仍在自行解析 JWT claim 取 uid（应统一走 raccoon.JWTUID，否则新版 sub 形态会漏）", f)
		}
	}
}

// TestRaccoonWebLoginConstants 网页版登录的关键常量必须与实盘抓包一致
// （2026-10-15 CDP 探针：登录态凭据 Cookie 名 xhx_token、域 .xiaohuanxiong.com）。
func TestRaccoonWebLoginConstants(t *testing.T) {
	src, err := os.ReadFile("../login_raccoon/web.go")
	if err != nil {
		t.Fatalf("读 web.go 失败: %v", err)
	}
	code := string(src)
	for _, want := range []string{
		`CookieName = "xhx_token"`,
		`CookieDomain = "xiaohuanxiong.com"`,
		`ownerTypeUsers = "users"`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("❌ web.go 缺少关键常量 %q", want)
		}
	}
}

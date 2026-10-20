package app

import (
	"os"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 本测试锁定 R44：小浣熊登录必须走「网页版 CDP 捕获」，
// 且**不得**再回到已废弃的桌面协议回调路径。
//
// 背景：原实现靠改写 HKCU 的 office-raccoon 协议注册接住深链授权码，
// 仅 Windows 可用、要求本机装客户端。2026-10-10 改为网页版流程（internal/login_raccoon/web.go）。
// 这类「接线」疏漏在源码层就能判定（同 R34「声明了却没接线」、R41 回调未接线家族），
// 故用静态断言锁死，避免将来回退。
//
// ⚠️ 2026-10-18 重写（深档排障 H5）—— 本测试**曾经是缺陷的一部分**：
// 它用整文件 strings.Contains 断言 `loginraccoon.Start()`（**无参**）必须出现，
// 而 app.go:738 那处串写缺陷恰好满足该字面量 ⇒ 带着 H1/H2 一路全绿。
// 它还把 `loginraccoon.Start(a.loginClient` 列为**废弃签名**严令禁止，
// 等于把「本渠道签名必须与其它 12 个渠道不同」这个架构缺陷写进了门禁 ——
// 逼着后续维护者只能把它挤进别人的 case 里（正是 H2 的成因）。
//
// 现在拆分职责：结构性问题（空壳 case / 串写覆盖）归 login_branch_ast_test.go 用 AST 判定；
// 本测试只保留独有职责 —— R44 的「不得回退到桌面协议回调」。
func TestRaccoonLoginUsesWebFlow(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	// 去行注释后再扫：本文件里刻意保留了描述旧缺陷的注释（教训留档），
	// 不能让注释里的字样参与断言（同 TestRaccoonUIDExtractionSingleSourceOfTruth 的做法）。
	var live []string
	for _, ln := range strings.Split(string(src), "\n") {
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		live = append(live, ln)
	}
	code := strings.Join(live, "\n")

	// ① 网页版入口必须被接线：直接查单一来源表。
	//    2026-10-18 收表后，调用已从 StartLoginFor 的 switch 移入 loginFlows 表，
	//    源码文本比对不再适用（它本来就分不清空壳 case 与死 case —— 见下）。
	f, ok := loginFlows[provider.Raccoon]
	if !ok {
		t.Fatal("❌ loginFlows 未登记 provider.Raccoon —— 小浣熊登录未接线")
	}
	if f.Start == nil {
		t.Error("❌ provider.Raccoon 没有 Start —— 网页版登录无法发起（H1 形态：空壳）")
	}
	if f.Poll == nil {
		t.Error("❌ provider.Raccoon 没有 Poll —— 登录结果无法回传")
	}
	if f.Shutdown == nil {
		t.Error("❌ provider.Raccoon 没有 Shutdown —— 浏览器不会被关闭")
	}

	// ② 不得回退到已废弃的**桌面协议回调**路径（R44 的核心约束）。
	//    判据特意**不再**看签名形态：旧版本把「Start 带参数」列为违规，
	//    与「全渠道签名同构」正面冲突，正是把架构缺陷写进门禁的那一手。
	//    现在只扫废弃实现的实体符号。
	for _, stale := range []string{
		"office-raccoon",
		"raccoon.NewClient()",
		"raccoonProto",
	} {
		if strings.Contains(code, stale) {
			t.Errorf("❌ app.go 仍引用已废弃的桌面协议回调实现 %q —— R44 已改用网页版 CDP 捕获", stale)
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

package app

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 本测试锁定「Loomy 的每日积分领取已接线且不会再静默失效」（2026-10-17）。
//
// 为什么需要它（这一轮真实踩到的坑）：
//
//	Loomy 的「宠物每日打工奖励」需要服务端业务日，于是上一轮的设计是「另立一套带业务日的
//	接口 + 改造调度器」。但调度器那段接线**从未落地**，结果：渠道里写好了 checkin.go、
//	却没有任何入口能调到它 —— 面板按钮被 noExplicitCheckin 挡着、定时任务全关、
//	连调度器的 refresh-token 前置判定也会把长会话账号提前挡回。**四个环节缺一个，
//	功能就等于没做，而且不报错**。故这里把四个环节逐条锁死。
func TestLoomyCheckinWiring(t *testing.T) {
	// ① 渠道实现：必须实现标准签到契约（provider.CheckinReporter 的 1 参方法）。
	cli, err := os.ReadFile("../loomy/checkin.go")
	if err != nil {
		t.Fatalf("读 internal/loomy/checkin.go 失败（签到实现缺失？）: %v", err)
	}
	src := string(cli)
	for _, want := range []string{
		"func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {",
		"func (c *Client) ClaimPetWorkReward(a *auth.Auth, businessDate string) (provider.CheckinReport, error) {",
		"func (c *Client) BusinessDateOf(a *auth.Auth) (string, error) {",
		"CheckinClaimed",
		"CheckinAlready",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("❌ loomy/checkin.go 缺少 %q —— 签到实现不完整", want)
		}
	}
	// ①b 不得再出现「那套没有调用方的带业务日接口」的**代码**（注释里解释历史不算）。
	// 判据用声明与适配器的代码形态，避免把说明性注释也当成违规。
	code := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(src, "")
	if strings.Contains(code, "type DatedCheckinReporter") ||
		strings.Contains(code, "func (c *Client) DailyCheckinReportFor(") {
		t.Error("❌ 又出现了 DatedCheckinReporter / DailyCheckinReportFor 的代码 —— " +
			"那套接口没有调用方，等于签到没有入口；请统一走 provider.CheckinReporter（1 参），" +
			"业务日由实现内部取")
	}

	// ② 后端名单：noExplicitCheckin 必须已移除 Loomy（否则手动按钮永久置灰、批量签到跳过）。
	if noExplicitCheckin(provider.Loomy) {
		t.Error("❌ noExplicitCheckin(Loomy) 仍为 true —— 面板手动签到按钮会被禁用，" +
			"批量签到也会跳过该渠道")
	}

	// ③ 前端名单：NO_EXPLICIT_CHECKIN 不得再包含 "loomy"。
	js, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败: %v", err)
	}
	if setLiteralContains(string(js), "NO_EXPLICIT_CHECKIN", "loomy") {
		t.Error("❌ app.js 的 NO_EXPLICIT_CHECKIN 仍含 \"loomy\" —— 前端签到按钮会被置灰")
	}

	// ④ 定时签到必须真的被打开（R39：传显式空切片 = 本渠道无此类任务）。
	//    只加渠道实现而不配调度器，是最隐蔽的一种「功能没生效」。
	mainSrc, err := os.ReadFile("../../cmd/wild-work/main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	main := string(mainSrc)
	for _, tc := range []struct {
		name string
		re   *regexp.Regexp
	}{
		{"loomy", regexp.MustCompile(`(?s)Name: "loomy",\s*CheckinMinutes: checkinMinutes`)},
		{"monkeycode", regexp.MustCompile(`(?s)Name: "monkeycode",\s*CheckinMinutes: checkinMinutes`)},
	} {
		if !tc.re.MatchString(main) {
			t.Errorf("❌ %s 的调度器未启用定时签到（CheckinMinutes 应为 checkinMinutes）—— "+
				"渠道实现了签到但定时任务没开，等于只能靠用户手点", tc.name)
		}
	}

	// ⑤ 登录/导入收尾必须做一次首次签到（否则新账号要等到下一个签到时刻才落账）。
	appSrc, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	if !strings.Contains(string(appSrc), "a.firstCheckinLoomy(") {
		t.Error("❌ app.go 未调用 firstCheckinLoomy —— Loomy 登录成功后不做首次领取")
	}

	// ⑥ CancelLogin 里每个渠道的关闭分支必须**平铺在函数体同一层**（1 个 Tab）。
	//    本轮真实踩到：Loomy 的 Shutdown 被误插进 Raccoon 的 if 块内部 ⇒ 只有取消
	//    小浣熊登录时才会执行，取消 Loomy 时引导页不关（端口/页面残留）。编译与
	//    「日志看起来正常」都发现不了，只有看缩进/嵌套层级才看得出来，故静态锁死。
	assertCancelLoginFlat(t, string(appSrc))
}

// assertCancelLoginFlat 断言 CancelLogin 内所有 `if kind == provider.X {` 都在函数体一级。
func assertCancelLoginFlat(t *testing.T, src string) {
	t.Helper()
	start := strings.Index(src, "func (a *App) CancelLogin()")
	if start < 0 {
		t.Fatal("未找到 CancelLogin")
	}
	rest := src[start:]
	// 函数体到下一个顶层 func 为止（够用：CancelLogin 之后紧跟另一个 func）。
	if i := strings.Index(rest[1:], "\nfunc "); i > 0 {
		rest = rest[:i+1]
	}
	// 注意 `\s*$`：工作区是 CRLF，行尾有 `\r`，写成 `\{$` 会一个都匹配不到
	//（这个断言本身踩过一次，故留注释）。
	re := regexp.MustCompile(`(?m)^(\t*)if kind == provider\.\w+ \{\s*$`)
	matches := re.FindAllStringSubmatch(rest, -1)
	if len(matches) == 0 {
		t.Fatal("CancelLogin 里没找到任何 `if kind == provider.X {` —— 结构变了，请更新本断言")
	}
	for _, m := range matches {
		if len(m[1]) != 1 {
			t.Errorf("❌ CancelLogin 里有分支缩进 %d 个 Tab（应为 1）—— "+
				"它被嵌进了别的渠道的 if 块里，只有那个渠道取消时才会跑", len(m[1]))
		}
	}
}

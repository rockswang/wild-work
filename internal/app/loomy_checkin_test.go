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

	// ⑥ 取消登录时必须释放各渠道占用的本机资源。
	//    本轮真实踩到（改造前）：Loomy 的 Shutdown 被误插进 Raccoon 的 if 块内部 ⇒ 只有
	//    取消小浣熊登录时才会执行，取消 Loomy 时引导页不关（端口/页面残留）。编译与
	//    「日志看起来正常」都发现不了。
	//    2026-10-18 收表后，CancelLogin 不再有逐渠道 if（统一查 loginFlows 的 Shutdown），
	//    这类「插错嵌套层级」缺陷结构上已不可能发生；断言改为直接验证表内容。
	assertLoginShutdownWired(t, string(appSrc))
}

// assertLoginShutdownWired 断言「取消登录时会释放本机资源」的渠道都在表里登记了 Shutdown。
//
// 改造前用「CancelLogin 里 if 的缩进层级」来判断是否漏关（因为那时是 6 个平铺 if，
// 插错一层就漏关）。收表后该结构消失，等价的不变式变成：凡是 start 时会占用本机资源
// （浏览器 / 127.0.0.1 回调监听）的渠道，其 loginFlow.Shutdown 必须非 nil。
func assertLoginShutdownWired(t *testing.T, src string) {
	t.Helper()
	if !strings.Contains(src, "a.shutdownLoginFlow(") {
		t.Fatal("❌ CancelLogin/pollLogin 未调用 shutdownLoginFlow —— 取消登录时不释放资源")
	}
	// 已知占用本机资源的渠道清单（新增此类渠道时必须同时登记 Shutdown）。
	for _, k := range []provider.Kind{
		provider.Raccoon,   // 独立 profile 的浏览器
		provider.Loomy,     // 127.0.0.1 登录引导页
		provider.LobsterAI, // 127.0.0.1 回调监听
		provider.Gemini,    // loopback OAuth 监听
		provider.CodeArts,  // PKCE 回调监听
		provider.QwenWork,  // 本机回调 server
	} {
		f, ok := loginFlows[k]
		if !ok {
			t.Errorf("❌ loginFlows 未登记 provider.%s", k)
			continue
		}
		if f.Shutdown == nil {
			t.Errorf("❌ provider.%s 的 loginFlow 没有 Shutdown —— 取消登录时它占用的本机资源不会被释放", k)
		}
	}
}

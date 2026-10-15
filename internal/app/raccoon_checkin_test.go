package app

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 本测试锁定 R65：小浣熊签到接线（setting_info 触发每日积分惰性落账）。
//
// 背景：上游没有独立领取端点，每日 300 积分的落账靠「当天第一次访问工作台接口」触发
// （见 internal/raccoon/checkin.go 的实测记录）。这次改动横跨四处：
//
//	① internal/raccoon/checkin.go   渠道实现（DailyCheckinReport）
//	② cmd/wild-work/main.go         调度器给签到时间 + 观察器
//	③ internal/app/app.go           noExplicitCheckin 移除 Raccoon（开放手动按钮）
//	④ cmd/wild-work/web/app.js      NO_EXPLICIT_CHECKIN 移除 "raccoon"
//
// 漏任意一处都**不报错**——只表现为「按钮是灰的」或「定时不跑」（R34/R41 同族），
// 故用静态断言锁死。
func TestRaccoonCheckinWiring(t *testing.T) {
	// ① 渠道实现：必须实现 provider.CheckinReporter（结构化状态），
	//    且**不得**退回「无签到活动」的旧桩。
	cliSrc, err := os.ReadFile("../raccoon/checkin.go")
	if err != nil {
		t.Fatalf("读 internal/raccoon/checkin.go 失败（签到实现缺失？）: %v", err)
	}
	cli := string(cliSrc)
	for _, want := range []string{
		"func (c *Client) DailyCheckinReport(",
		"func (c *Client) DailyCheckin(",
		"func (c *Client) FetchSettingInfo(",
		"EpOfficeSettingInfo",
		"CheckinClaimed",
		"CheckinAlready",
	} {
		if !strings.Contains(cli, want) {
			t.Errorf("❌ checkin.go 缺少 %q —— 签到实现不完整", want)
		}
	}
	oldStub, err := os.ReadFile("../raccoon/client.go")
	if err != nil {
		t.Fatalf("读 client.go 失败: %v", err)
	}
	if strings.Contains(string(oldStub), "raccoon 无签到活动") {
		t.Error("❌ client.go 仍保留「raccoon 无签到活动」的旧桩 —— 会与 checkin.go 的实现重复定义/语义冲突")
	}

	// ② noExplicitCheckin 必须已移除 Raccoon（否则手动签到按钮永久置灰、批量签到静默跳过）。
	if noExplicitCheckin(provider.Raccoon) {
		t.Error("❌ noExplicitCheckin(Raccoon) 仍为 true —— 面板手动签到按钮会被禁用，" +
			"批量签到也会跳过该渠道（App.CheckinAccount/CheckinAll 两处守卫）")
	}
	// 反向守门：真正无签到的渠道不得被误移除（防止将来「顺手删多了」）。
	for _, k := range []provider.Kind{
		provider.WorkBuddyAI, provider.QwenWork, provider.MonkeyCode,
		provider.Loomy, provider.Oczen, provider.Cline,
	} {
		if !noExplicitCheckin(k) {
			t.Errorf("❌ noExplicitCheckin(%s) 应为 true（该渠道确无签到活动）", k)
		}
	}

	// ③ 登录与导入两条添加路径都必须做首次签到（否则新账号要等下一个签到时刻才落账）。
	appSrc, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	if !strings.Contains(string(appSrc), "a.firstCheckinRaccoon(") {
		t.Error("❌ app.go 未调用 firstCheckinRaccoon —— 网页版登录后不做首次签到")
	}
	impSrc, err := os.ReadFile("import_local.go")
	if err != nil {
		t.Fatalf("读 import_local.go 失败: %v", err)
	}
	if !strings.Contains(string(impSrc), "a.firstCheckinRaccoon(") {
		t.Error("❌ import_local.go 未调用 firstCheckinRaccoon —— 从客户端导入后不做首次签到")
	}

	// ④ 调度器：rcSch 必须有签到时间（不再是显式空切片）且观察器已接线。
	mainSrc, err := os.ReadFile("../../cmd/wild-work/main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	main := string(mainSrc)
	rcRe := regexp.MustCompile(`(?s)rcSch := scheduler\.New\(scheduler\.Config\{[^}]*\}`)
	m := rcRe.FindString(main)
	if m == "" {
		t.Fatal("❌ main.go 未找到 rcSch 的 scheduler.New 定义")
	}
	if strings.Contains(m, "CheckinMinutes: []int{}") {
		t.Error("❌ rcSch 的 CheckinMinutes 仍为显式空切片 —— 定时签到不会运行（R39 语义：空切片 = 本渠道无此任务）")
	}
	if !strings.Contains(m, "CheckinMinutes: checkinMinutes") {
		t.Errorf("❌ rcSch 未使用全局签到时间，实际定义：%s", m)
	}
	for _, want := range []string{
		`rcSch.SetCheckinObserver(`,
		`rcSch.SetRefreshObserver(`,
	} {
		if !strings.Contains(main, want) {
			t.Errorf("❌ main.go 缺少 %q —— 签到结果不进日志，面板标签与 stats 签到收入都读不到", want)
		}
	}
}

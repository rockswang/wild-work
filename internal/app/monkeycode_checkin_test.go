package app

import (
	"os"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 本测试锁定「MonkeyCode 已实装签到」（2026-10-17）。
//
// 背景：MonkeyCode 此前被列为「无签到活动」（noExplicitCheckin + 前端 NO_EXPLICIT_CHECKIN），
// 桌面端国内版账号实际有签到领积分（POST {console}/api/v1/users/wallet/checkin，
// 见 internal/monkeycode/checkin.go）。开放签到要同时改若干处，漏任意一处都**不报错**：
//
//	① internal/monkeycode/checkin.go   渠道实现（DailyCheckinReport）+ 删除旧桩
//	② internal/app/app.go              noExplicitCheckin 移除 MonkeyCode（开放手动按钮）
//	③ cmd/wild-work/web/app.js         NO_EXPLICIT_CHECKIN 移除 "monkeycode"
//
// 只表现为「按钮是灰的」或「批量签到静默跳过」（R34/R41 同族），故用静态断言锁死。
func TestMonkeyCodeCheckinWiring(t *testing.T) {
	// ① 渠道实现：必须实现 provider.CheckinReporter（结构化状态）。
	cliSrc, err := os.ReadFile("../monkeycode/checkin.go")
	if err != nil {
		t.Fatalf("读 internal/monkeycode/checkin.go 失败（签到实现缺失？）: %v", err)
	}
	cli := string(cliSrc)
	for _, want := range []string{
		"func (c *Client) DailyCheckinReport(",
		"CheckinClaimed",
		"CheckinAlready",
	} {
		if !strings.Contains(cli, want) {
			t.Errorf("❌ monkeycode/checkin.go 缺少 %q —— 签到实现不完整", want)
		}
	}

	// ①b 旧桩必须已删除：client.go 里不得再有「无签到活动」的 DailyCheckin 实现，
	// 否则会与 checkin.go 重复定义或语义冲突。
	stubSrc, err := os.ReadFile("../monkeycode/client.go")
	if err != nil {
		t.Fatalf("读 internal/monkeycode/client.go 失败: %v", err)
	}
	if strings.Contains(string(stubSrc), "func (c *Client) DailyCheckin(_ *auth.Auth) error {") {
		t.Error("❌ monkeycode/client.go 仍保留 DailyCheckin 旧桩 —— 会与 checkin.go 的实现重复定义")
	}

	// ② 后端：noExplicitCheckin 必须已移除 MonkeyCode（否则手动按钮永久置灰、批量签到静默跳过）。
	if noExplicitCheckin(provider.MonkeyCode) {
		t.Error("❌ noExplicitCheckin(MonkeyCode) 仍为 true —— 面板手动签到按钮会被禁用，" +
			"批量签到也会跳过该渠道（App.CheckinAccount/CheckinAll 两处守卫）")
	}

	// ③ 前端：NO_EXPLICIT_CHECKIN 不得再包含 "monkeycode"。
	jsSrc, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败: %v", err)
	}
	if strings.Contains(string(jsSrc), `NO_EXPLICIT_CHECKIN = new Set([`) &&
		setLiteralContains(string(jsSrc), "NO_EXPLICIT_CHECKIN", "monkeycode") {
		t.Error("❌ app.js 的 NO_EXPLICIT_CHECKIN 仍含 \"monkeycode\" —— 前端签到按钮会被置灰")
	}
}

// setLiteralContains 判断 `<constName> = new Set([...])` 的字面量里是否含 want。
// 只做一次行内提取（限定在 Set([ … ]) 内），避免正则误匹配到同名注释。
func setLiteralContains(src, constName, want string) bool {
	key := constName + " = new Set(["
	i := strings.Index(src, key)
	if i < 0 {
		return false
	}
	rest := src[i+len(key):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return false
	}
	return strings.Contains(rest[:end], `"`+want+`"`)
}

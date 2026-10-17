package monkeycode

// live_test.go 对**真实上游**跑一次签到，用于人工确认「本实现与官方客户端同效」。
//
// 默认跳过（AGENTS §0.1：严禁自动化批打上游）。只在显式设置环境变量时运行：
//
//	WILDWORK_MC_LIVE=1
//	WILDWORK_MC_AUTH=<auths/monkeycode-*.json 的绝对路径>
//
// 用法（**单账号、单次**，专供发布前人工确认）：
//
//	WILDWORK_MC_LIVE=1 WILDWORK_MC_AUTH='C:\...\auths\monkeycode-xxx.json' \
//	  go test ./internal/monkeycode/ -run TestLiveCheckin -v
//
// ⚠️ 它会**真的消耗当日签到**（这正是期望行为：确认能领到积分）。
// 日志零 token——只打 uid / status / msg。

import (
	"os"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

func TestLiveCheckin(t *testing.T) {
	if os.Getenv("WILDWORK_MC_LIVE") != "1" {
		t.Skip("默认跳过：真实上游签到验证需显式 WILDWORK_MC_LIVE=1（AGENTS §0.1）")
	}
	path := os.Getenv("WILDWORK_MC_AUTH")
	if path == "" {
		t.Fatal("需同时设置 WILDWORK_MC_AUTH=<auths/monkeycode-*.json 绝对路径>")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 auth 文件失败：%v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("解析 auth 失败：%v", err)
	}
	if a.UID != "" {
		// 只打印尾 4 位，避免把完整账号标识写进日志
		uid := a.UID
		if len(uid) > 4 {
			uid = "…" + uid[len(uid)-4:]
		}
		t.Logf("uid=%s", uid)
	}

	c := New()
	rep, err := c.DailyCheckinReport(a)
	t.Logf("status=%s amount=%d msg=%q err=%v", rep.Status, rep.Amount, rep.Msg, err)

	switch rep.Status {
	case provider.CheckinClaimed, provider.CheckinAlready:
		// 领取成功，或今日已领——两者都说明与上游对话正常。
	default:
		t.Fatalf("真实上游签到未达成：status=%s msg=%s err=%v", rep.Status, rep.Msg, err)
	}
}

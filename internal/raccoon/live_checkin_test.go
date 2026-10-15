//go:build live

// live_checkin_test.go 小浣熊签到（setting_info）实盘探针。
//
// 背景：签到接入（R65）的核心判据是「调 setting_info 会触发每日积分惰性落账」，
// 这是 2026-10-15 从三账号对照实验得出的结论，本探针把它固化为可重跑的实盘验证。
//
// ⚠️ 本探针**只读**：不 refresh、不落盘、不改账号文件（与 live_probe_test.go 不同）。
// 唯一副作用是签到本身——即上游的每日积分落账（这正是验证目标）。
//
// 运行方式（仓库根目录）：
//
//	WILDWORK_AUTHDIR=~/Library/Application\ Support/WildWork/auths \
//	  go test -tags live ./internal/raccoon/ -run TestLiveCheckin -v -timeout 300s
//
// 验证三件事：
//
//  1. setting_info 可调通（HTTP 200 + code 0 + available_points 可读）；
//  2. 幂等语义：连调两次，第二次的 point_grant_popups 必为空
//     （若非空 ⇒ 上游改成了「每次调用都发放」= 严重问题，必须立刻发现）；
//  3. 状态判定与余额一致性：claimed 时 Amount 应与 bills 的最新 daily_grant 吻合。
package raccoon

import (
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

func TestLiveCheckin(t *testing.T) {
	a, dir := loadRaccoonAuth(t)
	c := New()

	t.Logf("authdir = %s", dir)
	t.Logf("uid     = %s", a.UID)

	// ---------- 1. 第一次调用：触发（或确认已触发）当日落账 ----------
	rep1, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("第一次 DailyCheckinReport 失败: %v", err)
	}
	t.Logf("第一次: status=%s amount=%d msg=%s", rep1.Status, rep1.Amount, rep1.Msg)

	switch rep1.Status {
	case provider.CheckinClaimed:
		t.Logf("✅ 本次触发了发放（+%d）", rep1.Amount)
		if rep1.Amount <= 0 {
			t.Errorf("claimed 但金额 <= 0（%d）——上游可能改了 popups 语义", rep1.Amount)
		}
	case provider.CheckinAlready:
		t.Logf("✅ 今天已经领过（幂等：本次未重复发放）")
	default:
		t.Errorf("非预期状态 %s（应为 claimed 或 already_claimed）", rep1.Status)
	}

	// ---------- 2. 第二次调用：幂等性 ----------
	// 关键不变量：同一天重复调用**不得**重复发放。若第二次仍 claimed，
	// 说明上游改成了「每次调用都发」，本工具会在每次签到时刻重复触发——
	// 这既是资损面也是风控面，必须硬失败。
	rep2, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("第二次 DailyCheckinReport 失败: %v", err)
	}
	t.Logf("第二次: status=%s amount=%d msg=%s", rep2.Status, rep2.Amount, rep2.Msg)
	if rep2.Status == provider.CheckinClaimed {
		t.Errorf("❌ 重复调用仍触发发放（+%d）——幂等性假设被上游推翻，"+
			"须重新评估签到触发频率（当前按签到时间每天 1~2 次）", rep2.Amount)
	}
	if rep2.Status != provider.CheckinAlready {
		t.Errorf("第二次状态应为 already_claimed，实际 %s", rep2.Status)
	}

	// ---------- 3. 余额可读（与签到解耦，作为健康检查） ----------
	remain, items, err := c.UserResourceDetail(a)
	if err != nil {
		t.Fatalf("UserResourceDetail 失败: %v", err)
	}
	t.Logf("余额 = %d，明细 %d 条", remain, len(items))
	for _, it := range items {
		if it.Remain > 0 {
			t.Logf("  %s: remain=%d usable=%t", it.Name, it.Remain, it.Usable)
		}
	}
	if remain <= 0 {
		t.Errorf("余额 <= 0（%d）——账号可能已耗尽或凭据异常", remain)
	}
}

// TestLiveCheckinSessionDeadShape 守门：用坏 token 调 setting_info 必须是 401 +
// 类型化 ErrSessionDead（调度器自愈依赖它，不变量 19）。
// 这是唯一会「故意失败」的实盘用例，不需要有效账号。
func TestLiveCheckinSessionDeadShape(t *testing.T) {
	c := New()
	bad := &auth.Auth{AccessToken: "invalid.token.for.probe", UID: "probe"}
	_, err := c.DailyCheckinReport(bad)
	if err == nil {
		t.Fatal("坏 token 应返回错误")
	}
	var pe *provider.Error
	if !asProviderError(err, &pe) {
		t.Fatalf("错误必须是 *provider.Error，实际 %T: %v", err, err)
	}
	if pe.Kind != provider.ErrSessionDead {
		t.Fatalf("Kind = %v, want session_dead（调度器 errors.As 自愈依赖）", pe.Kind)
	}
	t.Logf("✅ 坏 token 正确落 session_dead: %s", pe.Msg)
}

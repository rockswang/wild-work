// daily_grant_test.go 回归测试：硬冷却截到「每日发奖窗口 + 缓冲」。
//
// 背景（2026-10-17 现场）：qwenwork 账号 19:07 撞上游 402（余额不足以支付该模型），
// 旧逻辑按固定 cooldown.hard_credit=12h 锁到次日 07:07；但 qwenwork 每日 00:00
// （UTC+8）服务端自动发额度，账本 22 次发放实测落在 00:01~00:20。于是 00:20 就
// 到账的额度被白锁 7 小时。本组测试锁定「截到发奖窗口 + 缓冲」的口径：
//  1. 当日窗口未过 → 截到当日窗口；
//  2. 当日窗口已过 → 顺延次日窗口；
//  3. 截断只会缩短，不超过 hard_credit 上限；
//  4. 未配置窗口时行为与旧版完全一致（完整 hard_credit）。
package config

import (
	"testing"
	"time"
)

func mustConfig(t *testing.T) *Config {
	t.Helper()
	c := Default()
	c.Cooldown.DailyGrantAt = "00:00"
	c.Cooldown.DailyGrantBuffer = "30m"
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return c
}

// TestHardCooldownForTruncatesToNextDailyGrant 核心用例：19:07 撞 402 应锁到
// 次日 00:30（约 5h23m），而不是 12h。
func TestHardCooldownForTruncatesToNextDailyGrant(t *testing.T) {
	c := mustConfig(t)
	now := time.Date(2026, 10, 17, 19, 7, 32, 0, hardCooldownLoc)
	got := c.HardCooldownFor(now)
	want := time.Date(2026, 10, 18, 0, 30, 0, 0, hardCooldownLoc).Sub(now)
	if got != want {
		t.Fatalf("19:07 的硬冷却 = %v，期望 %v（次日 00:30 解冻）", got, want)
	}
	if got >= 12*time.Hour {
		t.Fatalf("必须短于 12h 上限，实际 %v", got)
	}
}

// TestHardCooldownForAfterWindowRollsToNextDay 窗口已过（00:40）时应顺延到次日
// 00:30；但顺延后的距离 23h50m 超过 12h 上限，故实际取上限 12h（上限保护）。
func TestHardCooldownForAfterWindowRollsToNextDay(t *testing.T) {
	c := mustConfig(t)
	now := time.Date(2026, 10, 18, 0, 40, 0, 0, hardCooldownLoc)
	got := c.HardCooldownFor(now)
	if got != 12*time.Hour {
		t.Fatalf("顺延距离超过上限时应钳到 12h，实际 %v", got)
	}
	// 顺延本身的口径单独验（不经过上限钳制）
	next := NextDailyGrant(now, c.DailyGrantAt, c.DailyGrantBufferDur)
	wantNext := time.Date(2026, 10, 19, 0, 30, 0, 0, hardCooldownLoc)
	if !next.Equal(wantNext) {
		t.Fatalf("NextDailyGrant = %v，期望 %v", next, wantNext)
	}
}

// TestHardCooldownForNeverExceedsCap 上限保护：窗口比 hard_credit 更远时不得延长冷却。
func TestHardCooldownForNeverExceedsCap(t *testing.T) {
	c := mustConfig(t)
	c.HardCreditDur = time.Hour // 上限压到 1h
	// 距离次日 00:30 还有 5h23m > 1h 上限 ⇒ 应取 1h
	now := time.Date(2026, 10, 17, 19, 7, 0, 0, hardCooldownLoc)
	if got := c.HardCooldownFor(now); got != time.Hour {
		t.Fatalf("应被上限钳到 1h，实际 %v", got)
	}
}

// TestHardCooldownForWithoutWindowKeepsLegacy 未配置窗口时行为与旧版一致。
func TestHardCooldownForWithoutWindowKeepsLegacy(t *testing.T) {
	c := Default()
	c.Cooldown.DailyGrantAt = ""
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	now := time.Date(2026, 10, 17, 19, 7, 0, 0, hardCooldownLoc)
	if got := c.HardCooldownFor(now); got != 12*time.Hour {
		t.Fatalf("未配置窗口应退回完整 12h，实际 %v", got)
	}
}

// TestNextDailyGrantCoversLedgerWindow 账本实测最晚到账 00:20；缓冲 30m 必须
// 覆盖它，否则解冻后立刻再撞 402。
func TestNextDailyGrantCoversLedgerWindow(t *testing.T) {
	at, err := parseClockHM("00:00")
	if err != nil {
		t.Fatalf("parseClockHM: %v", err)
	}
	buf, _ := time.ParseDuration("30m")
	// 从 10-17 任意时刻出发，解冻点必须 ≥ 10-18 00:20（账本最晚到账）
	next := NextDailyGrant(time.Date(2026, 10, 17, 19, 7, 0, 0, hardCooldownLoc), at, buf)
	ledgerLatest := time.Date(2026, 10, 18, 0, 20, 0, 0, hardCooldownLoc)
	if next.Before(ledgerLatest) {
		t.Fatalf("解冻点 %v 早于账本最晚到账 %v，会空转撞 402", next, ledgerLatest)
	}
}

// TestParseClockHMRejectsInvalid 非法窗口配置必须报错（不能静默当成 0 点）。
func TestParseClockHMRejectsInvalid(t *testing.T) {
	for _, bad := range []string{"24:00", "12:60", "abc", "12", "12:00:00"} {
		if _, err := parseClockHM(bad); err == nil {
			t.Fatalf("%q 应报错", bad)
		}
	}
	for _, ok := range []string{"00:00", "23:59", "09:05"} {
		if _, err := parseClockHM(ok); err != nil {
			t.Fatalf("%q 应通过，实际 %v", ok, err)
		}
	}
}

// TestDefaultConfigEnablesDailyGrantWindow 默认配置必须启用（qwenwork 是主用渠道）。
func TestDefaultConfigEnablesDailyGrantWindow(t *testing.T) {
	c := Default()
	if c.Cooldown.DailyGrantAt != "00:00" || c.Cooldown.DailyGrantBuffer != "30m" {
		t.Fatalf("默认值应启用 00:00 + 30m，实际 at=%q buf=%q",
			c.Cooldown.DailyGrantAt, c.Cooldown.DailyGrantBuffer)
	}
}

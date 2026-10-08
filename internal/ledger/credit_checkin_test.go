package ledger

// credit_checkin_test.go 回归测试（issue #67）：WorkBuddy 每日签到包落在「明日到期」
// 的 key 上且提前一天以 r=0 建档，「发放即消耗」场景下同 key 差分 delta=0，
// 旧实现不产生 earn → 面板今日收入恒 0。
//
// 锁定两层防线：
//  1. DiffCredits 的 used 差值补丁：remain 持平但 used 上涨 → 发放额 = usedDelta 记 earn；
//  2. RecordCheckinEarn：签到回执的权威发放值直接入账（不依赖差分时序）。

import (
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// newTestLedger 建临时 Ledger（测试结束关闭句柄，避免 Windows 文件锁卡 TempDir 清理）。
func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(l.Close)
	return l
}

// grantAndConsumeItem 造一个「发放即消耗」的条目：remain 与上次持平、used 上涨
// （WorkBuddy 签到包的真实形态：昨日快照 r=0/u=0 → 今日签到发放 100 并被用掉 100）。
func grantAndConsumeItem(remain, used int64) []provider.ResourceItem {
	return []provider.ResourceItem{
		{Name: "CodeBuddy个人版国内运营裂变包", Total: remain + used, Used: used, Remain: remain,
			ExpireAt: "2026-10-09", Key: "CodeBuddy个人版国内运营裂变包|2026-10-09", Usable: true},
	}
}

// queryToday 按面板口径聚合今日流水（earn 正数）。
func queryToday(t *testing.T, l *Ledger) (earn, spend int64) {
	t.Helper()
	st := l.Query(1, func(uid string) (name, channel string) { return "u1", "workbuddy" })
	return st.Credit.Earn, st.Credit.Spend
}

// TestDiffCreditsGrantConsumeSameKey 签到包提前建档 + 发放即消耗：
// remain 0→0、used 0→100 → 必须记一条 earn(+100)，不再漏记。
func TestDiffCreditsGrantConsumeSameKey(t *testing.T) {
	l := newTestLedger(t)
	// 第一次刷新：包尚未发放（提前一天以 r=0 建档）
	l.DiffCredits("workbuddy", "u1", 100, grantAndConsumeItem(0, 0))
	// 第二次刷新：签到发放 100 且已被消耗（remain 持平、used +100）
	n := l.DiffCredits("workbuddy", "u1", 100, grantAndConsumeItem(0, 100))
	earn, _ := queryToday(t, l)
	if n == 0 || earn != 100 {
		t.Fatalf("「发放即消耗」必须记一条 earn(+100)：events=%d earn=%d", n, earn)
	}
}

// TestDiffCreditsNormalSpendUnaffected 正常消耗（无发放）不受补丁影响：
// 首刷记 baseline earn（既有设计）；二刷 remain 下降且未到期 → 仍是 spend，无新 earn。
func TestDiffCreditsNormalSpendUnaffected(t *testing.T) {
	l := newTestLedger(t)
	l.DiffCredits("workbuddy", "u1", 800, grantAndConsumeItem(800, 0))
	earn0, _ := queryToday(t, l)
	if earn0 != 800 {
		t.Fatalf("首刷 baseline earn=800，实际 %d", earn0)
	}
	l.DiffCredits("workbuddy", "u1", 700, grantAndConsumeItem(700, 100))
	earn, spend := queryToday(t, l)
	if earn != 800 { // 二刷不得新增 earn（含 used 差值补丁不应误触发）
		t.Fatalf("正常消耗不得产生新 earn，实际 %d", earn)
	}
	if spend != 100 {
		t.Fatalf("期望 spend=100，实际 %d", spend)
	}
}

// TestDiffCreditsZeroUsedChannelNoEffect used 恒 0 的渠道（qwenwork 等）：
// remain 持平 → 无任何事件（补丁零影响）。
func TestDiffCreditsZeroUsedChannelNoEffect(t *testing.T) {
	l := newTestLedger(t)
	l.DiffCredits("qwenwork", "u1", 500, grantAndConsumeItem(500, 0))
	if n := l.DiffCredits("qwenwork", "u1", 500, grantAndConsumeItem(500, 0)); n != 0 {
		t.Fatalf("无变化不应产生事件，实际 %d 条", n)
	}
}

// TestRecordCheckinEarn 权威入账通道：amount>0 写入 earn 流水、Note 带签到标注；
// amount<=0 与 nil 接收者安全跳过。
func TestRecordCheckinEarn(t *testing.T) {
	l := newTestLedger(t)
	if !l.RecordCheckinEarn("workbuddy", "u1", 100, 3428, "签到奖励") {
		t.Fatal("amount>0 应写入")
	}
	if l.RecordCheckinEarn("workbuddy", "u1", 0, 3428, "签到奖励") {
		t.Fatal("amount=0 应跳过")
	}
	var nilL *Ledger
	if nilL.RecordCheckinEarn("workbuddy", "u1", 100, 0, "") {
		t.Fatal("nil Ledger 应安全跳过")
	}
	earn, _ := queryToday(t, l)
	if earn != 100 {
		t.Fatalf("权威入账 earn=%d, want 100", earn)
	}
	found := false
	st := l.Query(1, func(uid string) (name, channel string) { return "u1", "workbuddy" })
	for _, e := range st.Credit.Entries {
		if e.Kind == "earn" && strings.Contains(e.Note, "签到") {
			found = true
		}
	}
	if !found {
		t.Fatal("earn 条目 Note 应含「签到」标注")
	}
}

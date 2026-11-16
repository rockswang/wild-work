package ledger

// credit_modelscope_expire_test.go —— 票 03 的账本侧回归护栏（ModelScope 魔粒）。
//
// 与 credit_raccoon_expire_test.go 同构，但条目名换成 ModelScope 的「可用魔粒」，
// 并且显式锁死**升级瞬间的零假流水**：拆分前是单条聚合条目（无到期时刻），
// 拆分后是同 Name、无 Key、各带到期时刻的多条 —— ledger 的按 key 聚合必须把它们
// 视作同一条，否则升级当轮会凭空产出 expire+earn 一对假流水。

import (
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestModelScopeSplitNoFalseFlow 升级护栏：聚合条目 → 拆分条目，零假流水。
//
// 拆分前后同键（Name="可用魔粒"、Key 为空），remain 之和相同；total/used 因拆分
// 归零（raccoon withDailyExpiry 同款口径）不得被账本误读成收支事件。
func TestModelScopeSplitNoFalseFlow(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 第 1 轮（拆分前，票 02 的产物）：单条聚合「可用魔粒」。
	before := []provider.ResourceItem{
		{Name: "可用魔粒", Total: 500, Used: 200, Remain: 300, Usable: true},
	}
	if n := l.DiffCredits("modelscope", "u1", 300, before); n != 1 {
		t.Fatalf("首次快照应记 1 条 earn（存量额度），got %d", n)
	}

	// 第 2 轮（拆分后，票 03 的产物）：同 Name、无 Key、各带到期时刻，ΣRemain 仍是 300。
	after := []provider.ResourceItem{
		{Name: "可用魔粒", Remain: 200, Total: 200, ExpireAt: "2099-01-01T10:00:00+08:00", Usable: true},
		{Name: "可用魔粒", Remain: 100, Total: 100, ExpireAt: "2099-01-02T10:00:00+08:00", Usable: true},
	}
	if n := l.DiffCredits("modelscope", "u1", 300, after); n != 0 {
		t.Fatalf("拆分前后同键零假流水，got %d 条事件", n)
	}
	if got := len(readCreditEntries(t, l)); got != 1 {
		t.Fatalf("流水条数 = %d, want 1（只有首次 baseline，不得凭空多出 expire/earn）", got)
	}
}

// TestModelScopeDailyExpireNotSpend 到期清零记 expire：最早一笔的到期时刻在上一轮
// 快照里、本轮已消失 ⇒ 余额下降归 expire，不归 spend（R107 同款「今日消耗虚高」）。
func TestModelScopeDailyExpireNotSpend(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	yesterday := time.Now().In(expireLoc).AddDate(0, 0, -1).Format(time.RFC3339)
	cur1 := []provider.ResourceItem{
		{Name: "可用魔粒", Remain: 300, Total: 300, ExpireAt: yesterday, Usable: true},
		{Name: "可用魔粒", Remain: 7700, Total: 7700, ExpireAt: "2099-01-02T10:00:00+08:00", Usable: true},
	}
	if n := l.DiffCredits("modelscope", "u1", 8000, cur1); n != 1 {
		t.Fatalf("首次快照应记 1 条 earn，got %d", n)
	}

	// 最早那笔到期清零：余额 8000 → 7700，聚合后的 Expire（上一轮快照）已过。
	cur2 := []provider.ResourceItem{
		{Name: "可用魔粒", Remain: 7700, Total: 7700, ExpireAt: "2099-01-02T10:00:00+08:00", Usable: true},
	}
	if n := l.DiffCredits("modelscope", "u1", 7700, cur2); n != 1 {
		t.Fatalf("到期清零应记 1 条，got %d", n)
	}

	entries := readCreditEntries(t, l)
	var last *CreditEntry
	for i := range entries {
		if entries[i].Kind != "earn" {
			last = &entries[i]
		}
	}
	if last == nil {
		t.Fatal("未产生 expire/spend 流水")
	}
	if last.Kind != "expire" {
		t.Fatalf("魔粒到期清零被记成 %q（want expire）——这正是面板把到期显示成「今日消耗」的根因", last.Kind)
	}
	if last.Amount != -300 {
		t.Fatalf("过期金额 = %d, want -300", last.Amount)
	}
}

// TestModelScopeRealSpendStillSpend 真实对话消耗（未到期）仍必须记 spend ——
// 与上一条配对，防修复把真实消耗也吞成 expire（过度归因护栏）。
func TestModelScopeRealSpendStillSpend(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	cur1 := []provider.ResourceItem{
		{Name: "可用魔粒", Remain: 8000, Total: 8000, ExpireAt: "2099-01-01T10:00:00+08:00", Usable: true},
	}
	if n := l.DiffCredits("modelscope", "u1", 8000, cur1); n != 1 {
		t.Fatalf("首次快照应记 1 条 earn，got %d", n)
	}
	cur2 := []provider.ResourceItem{
		{Name: "可用魔粒", Remain: 7998, Total: 7998, ExpireAt: "2099-01-01T10:00:00+08:00", Usable: true},
	}
	if n := l.DiffCredits("modelscope", "u1", 7998, cur2); n != 1 {
		t.Fatalf("真实消耗应记 1 条，got %d", n)
	}
	entries := readCreditEntries(t, l)
	var last *CreditEntry
	for i := range entries {
		if entries[i].Kind != "earn" {
			last = &entries[i]
		}
	}
	if last == nil || last.Kind != "spend" {
		t.Fatalf("未到期下降必须记 spend，got %+v", last)
	}
	if last.Amount != -2 {
		t.Fatalf("消耗金额 = %d, want -2", last.Amount)
	}
}

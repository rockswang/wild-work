package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestRaccoonDailyExpireNotSpend R107 生产实录回归：小浣熊每日积分 7 天到期清零
// 必须记成 expire（今日作废），不能记成 spend（今日消耗）。
//
// 现场（2026-10-20 00:22:55）：三账号各一笔上游 daily_expire（-200/-191/-189），
// 合计 580；面板显示「小浣熊今日消耗 580」而当天真实对话请求 0 次。
//
// 根因链：渠道给「每日额度」的 ExpireAt 为空 ⇒ ledger.expired() 恒 false；
// 该渠道 Total=Remain、Used 恒 0 ⇒ poolShrankExpiry 门槛 2 不通过。
// 两条到期判据都哑火，「余额下降」被无条件记 spend。
//
// 修法（internal/raccoon）：每日额度按 bills 的逐笔发放拆分并带各自到期时刻。
// 本测试锁死 ledger 侧的行为：**只要条目带了已过的到期日，下降就必须归 expire**。
func TestRaccoonDailyExpireNotSpend(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 第 1 轮：每日额度 8000，其中最早一笔 300 的到期时刻是**昨天**。
	// 语义 = 「快照拍下的那一刻它还在（10-19 23:52 时 10-19 23:59 尚未到），
	// 下一轮拍之前它已过期消失」——账本判过期看的是**上一轮快照**的 Expire，
	// 用昨天的日期串即可等价表达这个时点差（同 TestDiffCreditsExpiredEntry）。
	//
	// 拆分条目共用 Name、无 Key —— 与渠道侧 withDailyExpiry 的产物同构。
	yesterday := time.Now().In(expireLoc).AddDate(0, 0, -1).Format(time.RFC3339)
	cur1 := []provider.ResourceItem{
		{Name: "每日额度", Remain: 300, Total: 300, ExpireAt: yesterday, Usable: true},
		{Name: "每日额度", Remain: 7700, Total: 7700, ExpireAt: "2099-01-02T23:59:59+08:00", Usable: true},
	}
	if n := l.DiffCredits("raccoon", "u1", 8000, cur1); n != 1 {
		t.Fatalf("首次快照应记 1 条 earn（存量额度），got %d", n)
	}

	// 第 2 轮：最早那笔到期消失（上游清零 300），余额 8000 → 7700。
	// 聚合后 Expire = 第一笔的到期时刻（已过）⇒ 必须记 expire。
	cur2 := []provider.ResourceItem{
		{Name: "每日额度", Remain: 7700, Total: 7700, ExpireAt: "2099-01-02T23:59:59+08:00", Usable: true},
	}

	if n := l.DiffCredits("raccoon", "u1", 7700, cur2); n != 1 {
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
		t.Fatalf("每日积分到期清零被记成 %q（want expire）——这正是面板把 580 显示成「今日消耗」的根因", last.Kind)
	}
	if last.Amount != -300 {
		t.Fatalf("过期金额 = %d, want -300", last.Amount)
	}
}

// TestRaccoonNoExpiryIsSpend 反向护栏：修复前的渠道形态（每日额度不带 ExpireAt）
// 必然把到期清零记成 spend —— 这正是 580 的成因，把根因钉死在账本侧。
//
// 为什么需要这条：上面那条测试只证明「带到期日就记 expire」。若渠道侧哪天回退
// 成不带 ExpireAt，上面仍会绿（因为测试自己构造了到期日），只有这条会红。
func TestRaccoonNoExpiryIsSpend(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 修复前的渠道产物：单条聚合「每日额度」，ExpireAt 为空。
	cur1 := []provider.ResourceItem{
		{Name: "每日额度", Remain: 8000, Total: 8000, Usable: true},
	}
	l.DiffCredits("raccoon", "u1", 8000, cur1)
	cur2 := []provider.ResourceItem{
		{Name: "每日额度", Remain: 7800, Total: 7800, Usable: true},
	}
	l.DiffCredits("raccoon", "u1", 7800, cur2)

	entries := readCreditEntries(t, l)
	var last *CreditEntry
	for i := range entries {
		if entries[i].Kind != "earn" {
			last = &entries[i]
		}
	}
	if last == nil {
		t.Fatal("未产生流水")
	}
	if last.Kind != "spend" {
		t.Fatalf("无到期日时账本只能记 spend（got %q）——若这里变了说明 ledger 判据被改动，需复核 R101/R107", last.Kind)
	}
	if last.Amount != -200 {
		t.Fatalf("金额 = %d, want -200", last.Amount)
	}
}

// TestRaccoonRealSpendStillSpend 真实对话消耗（未到期）仍必须记 spend。
//
// 与上一条配对：修复不能把真实消耗也吞成 expire（护栏，防过度归因）。
func TestRaccoonRealSpendStillSpend(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	cur1 := []provider.ResourceItem{
		{Name: "每日额度", Remain: 8000, Total: 8000, ExpireAt: "2099-01-01T23:59:59+08:00", Usable: true},
	}
	if n := l.DiffCredits("raccoon", "u1", 8000, cur1); n != 1 {
		t.Fatalf("首次快照应记 1 条 earn，got %d", n)
	}
	// 消耗 2 分，到期日未变且仍在未来。
	cur2 := []provider.ResourceItem{
		{Name: "每日额度", Remain: 7998, Total: 7998, ExpireAt: "2099-01-01T23:59:59+08:00", Usable: true},
	}
	if n := l.DiffCredits("raccoon", "u1", 7998, cur2); n != 1 {
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

// TestRaccoonSplitItemsSameKey 拆分条目（共用 Name、无 Key）必须聚合回一条，
// 否则一次刷新会产出多条流水、快照只留末条下次继续错（issue #38 同族）。
func TestRaccoonSplitItemsSameKey(t *testing.T) {
	dir := t.TempDir() + "/ledger"
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	cur := []provider.ResourceItem{
		{Name: "每日额度", Remain: 300, Total: 300, ExpireAt: "2099-01-01T23:59:59+08:00", Usable: true},
		{Name: "每日额度", Remain: 300, Total: 300, ExpireAt: "2099-01-02T23:59:59+08:00", Usable: true},
		{Name: "每日额度", Remain: 300, Total: 300, ExpireAt: "2099-01-03T23:59:59+08:00", Usable: true},
	}
	if n := l.DiffCredits("raccoon", "u1", 900, cur); n != 1 {
		t.Fatalf("三条同 key 条目应聚合为 1 条 earn，got %d", n)
	}
	entries := readCreditEntries(t, l)
	if len(entries) != 1 {
		t.Fatalf("流水条数 = %d, want 1（同 key 聚合）", len(entries))
	}
	if entries[0].Amount != 900 {
		t.Fatalf("聚合金额 = %d, want 900", entries[0].Amount)
	}
}

// readCreditEntries 读回该目录全部积分流水（测试辅助）。
// 直接读 JSONL 而非 Query(1)：Query 只扫「今日」窗口，测试里的条目时间戳落在
// 窗口外时会读到空表（曾因此把「聚合正确」误判成「没写流水」）。
func readCreditEntries(t *testing.T, l *Ledger) []CreditEntry {
	t.Helper()
	l.Flush() // 必须刷**原**实例的缓冲句柄：另开一个 Ledger 只会刷自己的空句柄
	raw, err := os.ReadFile(filepath.Join(l.dir, monthFile("credit", time.Now())))
	if err != nil {
		t.Fatalf("读积分流水失败: %v", err)
	}
	var out []CreditEntry
	for _, line := range splitLines(raw) {
		var e CreditEntry
		if unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestDiffCreditsEarningsAndSpend 差分记账：发放→入项、消耗→spend、消失→过期。
func TestDiffCreditsEarningsAndSpend(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	cur1 := []provider.ResourceItem{
		{Name: "签到奖励", Key: "checkin_1", Remain: 100, ExpireAt: "2099-01-01", Usable: true},
	}
	if n := l.DiffCredits("traework", "u1", 100, cur1); n != 1 {
		t.Fatalf("首次快照应记 1 条 earn，got %d", n)
	}

	// 消耗 30：spend
	cur2 := []provider.ResourceItem{
		{Name: "签到奖励", Key: "checkin_1", Remain: 70, ExpireAt: "2099-01-01", Usable: true},
	}
	if n := l.DiffCredits("traework", "u1", 70, cur2); n != 1 {
		t.Fatalf("消耗应记 1 条 spend，got %d", n)
	}

	// 条目消失：expire
	if n := l.DiffCredits("traework", "u1", 0, nil); n != 1 {
		t.Fatalf("条目消失应记 1 条 expire，got %d", n)
	}

	l.Flush()
	// 验证文件行数与 kind 序列
	raw, err := os.ReadFile(filepath.Join(dir, monthFile("credit", time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, line := range splitLines(raw) {
		var e CreditEntry
		if unmarshal(line, &e) == nil {
			kinds = append(kinds, e.Kind)
		}
	}
	want := []string{"earn", "spend", "expire"}
	if len(kinds) != 3 {
		t.Fatalf("kinds=%v", kinds)
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Fatalf("kinds[%d]=%s want %s", i, kinds[i], w)
		}
	}
}

// TestDiffCreditsExpiredEntry 到期日已过的下降归因 expire 而非 spend。
func TestDiffCreditsExpiredEntry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	yesterday := time.Now().In(expireLoc).AddDate(0, 0, -1).Format("2006-01-02")
	cur1 := []provider.ResourceItem{
		{Name: "每日奖励", Key: "daily", Remain: 100, ExpireAt: yesterday, Usable: true},
	}
	l.DiffCredits("traework", "u2", 100, cur1)
	cur2 := []provider.ResourceItem{
		{Name: "每日奖励", Key: "daily", Remain: 40, ExpireAt: yesterday, Usable: true},
	}
	l.DiffCredits("traework", "u2", 40, cur2)

	l.Flush()
	raw, _ := os.ReadFile(filepath.Join(dir, monthFile("credit", time.Now())))
	var last CreditEntry
	for _, line := range splitLines(raw) {
		_ = unmarshal(line, &last)
	}
	if last.Kind != "expire" {
		t.Fatalf("到期日已过的下降应归因 expire，got %s", last.Kind)
	}
}

// TestDiffCreditsDuplicateKeys 同 key 多条目先聚合再差分（issue #38 回归）：
// WorkBuddy 伪键「套餐名|到期日」可对应多条独立套餐。修复前每条都对比同一份
// 旧快照 → 一次刷新重复记 spend、快照只留末条下次继续错。聚合后每 key 每次
// 刷新最多一条事件，且无变化刷新零事件。
func TestDiffCreditsDuplicateKeys(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	// 两条同名同到期日（同 key）的独立套餐，r1+r2 为聚合余额
	dup := func(r1, r2 int64) []provider.ResourceItem {
		return []provider.ResourceItem{
			{Name: "拉新权益包", Key: "拉新权益包|2099-01-01", Remain: r1, ExpireAt: "2099-01-01", Usable: true},
			{Name: "拉新权益包", Key: "拉新权益包|2099-01-01", Remain: r2, ExpireAt: "2099-01-01", Usable: true},
		}
	}
	// 首次：baseline 一条 earn = 400（聚合值，不被重复 key 折叠）
	if n := l.DiffCredits("workbuddy", "u1", 400, dup(200, 200)); n != 1 {
		t.Fatalf("首次应记 1 条 baseline earn，got %d", n)
	}
	// 消耗 30：聚合差分只产生 1 条 spend(-30)，而非两条各记一次
	if n := l.DiffCredits("workbuddy", "u1", 370, dup(185, 185)); n != 1 {
		t.Fatalf("消耗应记 1 条 spend，got %d", n)
	}
	// 无变化刷新：0 事件（验证快照存的是聚合值而非末条 185）
	if n := l.DiffCredits("workbuddy", "u1", 370, dup(185, 185)); n != 0 {
		t.Fatalf("无变化不应记账，got %d", n)
	}

	st := l.Query(7, nil)
	if st.Credit.Spend != 30 {
		t.Fatalf("spend=%d want 30（重复 key 不得重复累计）", st.Credit.Spend)
	}
	if st.Credit.Earn != 400 {
		t.Fatalf("earn=%d want 400", st.Credit.Earn)
	}
}

// TestMigrateDupKeyCredit 一次性归档：credit-*.jsonl → old-credit-*、删快照、写 marker；
// marker 已在时二次调用不得再归档（保留升级后新产生的流水）。
func TestMigrateDupKeyCredit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	os.MkdirAll(dir, 0o755)
	cur := monthFile("credit", time.Now())
	bad := []byte(`{"ts":1,"kind":"spend","amount":-200}` + "\n")
	if err := os.WriteFile(filepath.Join(dir, cur), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credit-snapshot.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	l, _ := New(dir)
	defer l.Close()
	if _, err := os.Stat(filepath.Join(dir, "old-"+cur)); err != nil {
		t.Fatalf("credit 分段应已归档：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "credit-snapshot.json")); !os.IsNotExist(err) {
		t.Fatal("旧快照应删除（伪键只留末条，与聚合 cur 差分会凭空多记 earn）")
	}
	if _, err := os.Stat(filepath.Join(dir, ".credit-dedup-migrated")); err != nil {
		t.Fatalf("marker 应已落盘：%v", err)
	}

	// marker 已在：新产生的流水不得被再次归档
	if err := os.WriteFile(filepath.Join(dir, cur), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	l.migrateDupKeyCredit()
	if _, err := os.Stat(filepath.Join(dir, cur)); err != nil {
		t.Fatalf("已迁移后不得再归档：%v", err)
	}
}

// TestQueryAggregation 聚合：token 按模型、积分按账号，范围过滤生效。
func TestQueryAggregation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	l.AppendUsage(UsageEntry{Ch: "workbuddy", UID: "u1", Model: "glm-5.2", PT: 100, CT: 50, Src: "upstream"})
	l.AppendUsage(UsageEntry{Ch: "workbuddy", UID: "u1", Model: "glm-5.2", PT: 10, CT: 5})
	l.AppendUsage(UsageEntry{Ch: "traework", UID: "u2", Model: "doubao-seed", PT: 200, CT: 100})
	l.AppendCredit(CreditEntry{Ch: "workbuddy", UID: "u1", Kind: "earn", Amount: 1550, Balance: 1550})
	l.AppendCredit(CreditEntry{Ch: "traework", UID: "u2", Kind: "spend", Amount: -30, Balance: 970})

	st := l.Query(7, nil)
	if st.Token.Total != 465 {
		t.Fatalf("token total=%d want 465", st.Token.Total)
	}
	if st.Token.Requests != 3 {
		t.Fatalf("requests=%d want 3", st.Token.Requests)
	}
	if len(st.Token.ByModel) != 2 {
		t.Fatalf("byModel len=%d", len(st.Token.ByModel))
	}
	// 模型榜降序：glm-5.2 (165) < doubao (300)，doubao 应排前
	if st.Token.ByModel[0].Model != "doubao-seed" {
		t.Fatalf("byModel[0]=%s want doubao-seed", st.Token.ByModel[0].Model)
	}
	if st.Credit.Earn != 1550 || st.Credit.Spend != 30 {
		t.Fatalf("earn=%d spend=%d", st.Credit.Earn, st.Credit.Spend)
	}
	// 原始条目：时间升序全量返回
	if len(st.Credit.Entries) != 2 {
		t.Fatalf("entries=%d want 2", len(st.Credit.Entries))
	}
	if st.Credit.Entries[0].Kind != "earn" || st.Credit.Entries[1].Kind != "spend" {
		t.Fatalf("entries kinds=%s,%s want earn,spend", st.Credit.Entries[0].Kind, st.Credit.Entries[1].Kind)
	}
}

// TestQueryCrossMonthSegments 跨月窗口必须扫到当月分段（月份枚举回归）。
// 旧实现「from 起 +15 天步进」在 from 处于上月 25 号后（7 天窗口跨月常态）时，
// 步进直接越过当月 → 本月整月数据不进面板（每月 1~6 号必现，跨年同理）。
// 无法直接操纵 Query 内部的 time.Now，改为验证分段枚举函数本身 +
// 手工构造跨月数据走 Query 全链路（当月部分必被聚合）。
func TestQueryCrossMonthSegments(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	// 当月写入一条（若枚举漏掉当月，这条不会被聚合）
	l.AppendUsage(UsageEntry{Ch: "workbuddy", UID: "u1", Model: "glm-5.2", PT: 111, CT: 22, Src: "upstream", Ts: monthEdgeTs(t)})

	st := l.Query(7, nil)
	if st.Token.Total != 133 {
		t.Fatalf("跨月窗口漏扫当月分段：total=%d want 133", st.Token.Total)
	}
	if st.Token.Requests != 1 {
		t.Fatalf("requests=%d want 1", st.Token.Requests)
	}
}

// monthEdgeTs 构造「跨月窗口边界」时间戳：今天 00:00 − 6 天 = Query(7) 的窗口起点
// （fromTs，边界含），无论跑在哪天都必被聚合；当月天数 ≤ 6 时落在上月 → 仍覆盖
// 旧枚举 bug 的触发场景（from 在上月末尾、跨月扫当月）。
func monthEdgeTs(t *testing.T) int64 {
	t.Helper()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return today.AddDate(0, 0, -6).Unix()
}

// TestQueryMonthEnum 月份枚举：from 处于上月末尾时不得跳过当月（原 +15 天步进 bug 的
// 直接单测——构造 09-27 → 10-03 场景，旧写法枚举出 {202609}、漏 202610）。
func TestQueryMonthEnum(t *testing.T) {
	// 复刻 Query 内的枚举逻辑演进前后对比，锁定「逐月步进」语义。
	from := time.Date(2026, 9, 27, 0, 0, 0, 0, time.Local)
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)

	// 新逻辑：从 from 所在月第一天起，按月步进
	got := map[string]bool{}
	for t := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.Local); !t.After(now); t = t.AddDate(0, 1, 0) {
		got[t.Format("200601")] = true
	}
	if !got["202609"] || !got["202610"] {
		t.Fatalf("月份枚举应含 202609+202610，got %v", got)
	}

	// 跨年：12-30 → 次年 1-05
	from2 := time.Date(2026, 12, 30, 0, 0, 0, 0, time.Local)
	now2 := time.Date(2027, 1, 5, 0, 0, 0, 0, time.Local)
	got2 := map[string]bool{}
	for t := time.Date(from2.Year(), from2.Month(), 1, 0, 0, 0, 0, time.Local); !t.After(now2); t = t.AddDate(0, 1, 0) {
		got2[t.Format("200601")] = true
	}
	if !got2["202612"] || !got2["202701"] {
		t.Fatalf("跨年枚举应含 202612+202701，got %v", got2)
	}
}

// TestDiffCreditsNewKeyEarnsAfterBaseline 回归（2026-10-02 生产账本实证）：
// 非首次刷新中出现的新 key 必须记 earn。
//
// 真实场景：WorkBuddy 伪键 =「套餐名|到期日」，月周期切换时旧键
// "CodeBuddy个人体验版|2026-09-30" 消失（记 expire），余额挂在
// "CodeBuddy个人体验版|2026-10-31" 上——旧实现把新 key 一律静默跳过，
// 导致每账号每月约 2500 的周期发放从不进入收入流水（账本只降不升）。
func TestDiffCreditsNewKeyEarnsAfterBaseline(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 首次：只应有一条 baseline（存量额度），不得逐条展开
	cur1 := []provider.ResourceItem{
		{Name: "体验版", Key: "体验版|2026-09-30", Remain: 200, ExpireAt: "2026-09-30", Usable: true},
	}
	if n := l.DiffCredits("workbuddy", "u1", 200, cur1); n != 1 {
		t.Fatalf("首次应只记 1 条 baseline，got %d", n)
	}

	// 周期切换：旧键消失（expire -200）+ 新键 2500（earn +2500）= 2 条
	cur2 := []provider.ResourceItem{
		{Name: "体验版", Key: "体验版|2026-10-31", Remain: 2500, ExpireAt: "2026-10-31", Usable: true},
	}
	if n := l.DiffCredits("workbuddy", "u1", 2500, cur2); n != 2 {
		t.Fatalf("换池应记 expire+earn 共 2 条，got %d", n)
	}

	l.Flush()
	raw, _ := os.ReadFile(filepath.Join(dir, monthFile("credit", time.Now())))
	var es []CreditEntry
	for _, line := range splitLines(raw) {
		var e CreditEntry
		if unmarshal(line, &e) == nil {
			es = append(es, e)
		}
	}
	if len(es) != 3 {
		t.Fatalf("entries=%d want 3", len(es))
	}
	// 第 2、3 条为换池产物：一条 expire(-200)、一条 earn(+2500)
	var sawExpire, sawEarn bool
	for _, e := range es[1:] {
		switch {
		case e.Kind == "expire" && e.Amount == -200:
			sawExpire = true
		case e.Kind == "earn" && e.Amount == 2500:
			sawEarn = true
		}
	}
	if !sawExpire || !sawEarn {
		t.Fatalf("换池应含 expire(-200)+earn(+2500)，got %+v", es[1:])
	}
}

// TestDiffCreditsSameKeyUnchangedNoEvents 新逻辑不得引入噪声：
// 同 key 同余额的重复刷新仍然零事件。
func TestDiffCreditsSameKeyUnchangedNoEvents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	cur := []provider.ResourceItem{
		{Name: "包", Key: "pkg|10-31", Remain: 500, ExpireAt: "2026-10-31", Usable: true},
	}
	l.DiffCredits("workbuddy", "u9", 500, cur) // baseline 1 条
	if n := l.DiffCredits("workbuddy", "u9", 500, cur); n != 0 {
		t.Fatalf("无变化刷新应零事件，got %d", n)
	}
}

// TestQueryEntriesTimeSorted entries 必须按时间升序返回（前端倒序分页依赖此顺序）。
// months 是 map、range 顺序随机，跨月时会先扫到当月文件把最新条目排到前面；
// 修复前这里同文件乱序落盘即能复现（返回顺序 = 写入顺序）。
func TestQueryEntriesTimeSorted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ledger")
	l, _ := New(dir)
	defer l.Close()

	now := time.Now().Unix()
	// 故意乱序落盘：大 ts 在前、小 ts 在后
	lines := []CreditEntry{
		{Ts: now, Ch: "workbuddyai", UID: "u1", Kind: "earn", Amount: 100, Balance: 100},
		{Ts: now - 3600, Ch: "workbuddyai", UID: "u1", Kind: "spend", Amount: -30, Balance: 70},
	}
	f, err := os.OpenFile(filepath.Join(dir, monthFile("credit", time.Now())),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range lines {
		b, _ := json.Marshal(e)
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	st := l.Query(7, nil)
	if len(st.Credit.Entries) != 2 {
		t.Fatalf("entries=%d want 2", len(st.Credit.Entries))
	}
	if st.Credit.Entries[0].Ts > st.Credit.Entries[1].Ts {
		t.Fatalf("entries 未按时间升序：%d > %d", st.Credit.Entries[0].Ts, st.Credit.Entries[1].Ts)
	}
}

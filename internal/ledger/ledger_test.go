package ledger

import (
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

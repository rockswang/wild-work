package ledger

// credit_poolshrink_test.go 回归测试（R101）：余额下降的两种来源必须分开归因。
//
// 生产实录（2026-10-19）：QoderCN 一个 100 分赠送包整包到期消失，它此前被花掉 1 分。
// 上游回包 total 2500→2400、used 1→0、remaining 2499→2400，旧实现只比较 remain，
// 「无到期日 + 下降」⇒ 恒记 spend ⇒ 面板显示「今日消耗 99」，实际当天真消耗是 0。
//
// 判据：remain = total - used ⇒ Δremain = Δtotal - Δused。
// 「花掉」是 used 涨，total 不动；「池子被回收/到期」是 total 缩小。
//
// 同时用测试钉住三条护栏（去掉任意一条都会把某类渠道的真实消耗误记成过期）：
//  1. 有到期日的条目仍走 expired() 既有判据；
//  2. 不区分 total/used 的渠道（Total=Remain、Used 恒 0）不受影响；
//  3. 老快照没有 total 字段（零值）时退回旧口径，不改写历史归因。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// shrinkItem 造一个「三字段独立上报、无到期日」的条目（QoderCN/QoderCOM 的真实形态）。
func shrinkItem(total, used, remain int64) []provider.ResourceItem {
	return []provider.ResourceItem{
		{Name: "赠送额度", Total: total, Used: used, Remain: remain, Key: "addOnQuota", Usable: true},
	}
}

// lastEntry 返回当月 credit 流水最后一条（判断最新归因用）。
// 写入侧是带缓冲句柄（8KB bufio），读文件前必须先 Flush。
func lastEntry(t *testing.T, l *Ledger, dir string) CreditEntry {
	t.Helper()
	l.Flush()
	raw, err := os.ReadFile(filepath.Join(dir, monthFile("credit", time.Now())))
	if err != nil {
		t.Fatalf("读流水失败：%v", err)
	}
	var last CreditEntry
	for _, line := range splitLines(raw) {
		_ = unmarshal(line, &last)
	}
	return last
}

// TestDiffCreditsPoolShrinkIsExpire 复刻 2026-10-19 的真实数字：
// total 2500→2400、used 1→0、remain 2499→2400 ⇒ 必须记 expire(-99)，不得记 spend。
func TestDiffCreditsPoolShrinkIsExpire(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 首刷：存量 baseline（一条 earn 合计 2499）
	l.DiffCredits("qodercn", "u1", 2499, shrinkItem(2500, 1, 2499))
	// 二刷：整包到期消失，remain 掉 99
	if n := l.DiffCredits("qodercn", "u1", 2400, shrinkItem(2400, 0, 2400)); n != 1 {
		t.Fatalf("应记 1 条事件，got %d", n)
	}

	last := lastEntry(t, l, dir)
	if last.Kind != "expire" {
		t.Fatalf("池子缩水导致的下滑必须记 expire，got %s", last.Kind)
	}
	if last.Amount != -99 {
		t.Fatalf("amount=%d want -99", last.Amount)
	}
	st := l.Query(1, nil)
	if st.Credit.Spend != 0 {
		t.Fatalf("真实消耗为 0，spend 必须为 0，got %d", st.Credit.Spend)
	}
	if st.Credit.Expire != 99 {
		t.Fatalf("expire=%d want 99", st.Credit.Expire)
	}
}

// TestDiffCreditsRealSpendStillSpend 反向守门（护栏 3）：池子没缩、只是用掉更多，
// 必须仍是 spend —— 否则所有 Qoder 系的真实消耗都会被误记成过期。
func TestDiffCreditsRealSpendStillSpend(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir)
	defer l.Close()

	l.DiffCredits("qodercn", "u2", 2500, shrinkItem(2500, 0, 2500))
	l.DiffCredits("qodercn", "u2", 2401, shrinkItem(2500, 99, 2401)) // total 不动、used +99

	last := lastEntry(t, l, dir)
	if last.Kind != "spend" || last.Amount != -99 {
		t.Fatalf("真实消耗必须记 spend -99，got %s %d", last.Kind, last.Amount)
	}
}

// TestDiffCreditsPoolShrinkWithExpiryUntouched 护栏 1：有到期日的条目完全走
// expired() 既有判据，本补丁零影响。两个方向都锁：
//   - 未到期却 total 缩水（唯一能同时满足门槛 2/3）⇒ 仍必须是旧口径 spend，
//     不得被新规则抢成 expire；
//   - 已到期 ⇒ expire（既有行为不变）。
//
// 测试条目必须带 used>0：否则新增的两个门槛会先把这条拦下，测不到门槛 1 本身。
func TestDiffCreditsPoolShrinkWithExpiryUntouched(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir)
	defer l.Close()

	future := time.Now().In(expireLoc).AddDate(0, 0, 7).Format("2006-01-02")
	past := time.Now().In(expireLoc).AddDate(0, 0, -1).Format("2006-01-02")
	withExp := func(exp string, total, used, remain int64) []provider.ResourceItem {
		return []provider.ResourceItem{
			{Name: "签到包", Total: total, Used: used, Remain: remain,
				ExpireAt: exp, Key: "pkg", Usable: true},
		}
	}
	// 未到期：total 100→90、used 恒 5、remain 95→85（池子缩水 10，但包尚未过期）
	l.DiffCredits("traework", "u3", 95, withExp(future, 100, 5, 95))
	l.DiffCredits("traework", "u3", 85, withExp(future, 90, 5, 85))
	if last := lastEntry(t, l, dir); last.Kind != "spend" {
		t.Fatalf("有到期日的条目不得被本补丁改动，want spend got %s", last.Kind)
	}

	// 已到期：走 expired() 既有判据 ⇒ expire
	l.DiffCredits("traework", "u6", 95, withExp(past, 100, 5, 95))
	l.DiffCredits("traework", "u6", 85, withExp(past, 100, 15, 85))
	if last := lastEntry(t, l, dir); last.Kind != "expire" {
		t.Fatalf("已到期条目的下滑仍应记 expire，got %s", last.Kind)
	}
}

// TestDiffCreditsNoUsedChannelUnaffected 护栏 2：qwenwork/raccoon 这类
// 「Total 填成 Remain、Used 恒 0」的渠道，每次真实消耗都让 Total 一起下降。
// 若没有 used>0 门槛，它们的消耗会全被误记成过期。
func TestDiffCreditsNoUsedChannelUnaffected(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir)
	defer l.Close()

	wallets := func(remain int64) []provider.ResourceItem {
		// 真实形态：Total == Remain，Used 恒 0，且无到期日（longterm 钱包）
		return []provider.ResourceItem{
			{Name: "长期积分", Total: remain, Used: 0, Remain: remain, Key: "longterm", Usable: true},
		}
	}
	l.DiffCredits("qwenwork", "u4", 1188, wallets(1188))
	l.DiffCredits("qwenwork", "u4", 1100, wallets(1100))

	if last := lastEntry(t, l, dir); last.Kind != "spend" {
		t.Fatalf("不区分 total/used 的渠道必须仍记 spend，got %s", last.Kind)
	}
}

// TestDiffCreditsOldSnapshotNoTotal 护栏 3（不变量 5 向后兼容）：升级前落盘的快照
// 没有 t 字段（零值）。首刷不得凭零值判「total 下降」，必须退回旧口径 spend；
// 而且这次刷新要把 total 补进快照，下一轮才真正生效。
func TestDiffCreditsOldSnapshotNoTotal(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir)
	defer l.Close()

	// 手工造一份老格式快照：只有 k/r/u，没有 t
	old := `{"qodercn/u5":[{"k":"addOnQuota","r":2499,"u":1}]}`
	if err := os.WriteFile(filepath.Join(dir, "credit-snapshot.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	l.DiffCredits("qodercn", "u5", 2400, shrinkItem(2400, 0, 2400))

	if last := lastEntry(t, l, dir); last.Kind != "spend" {
		t.Fatalf("老快照（无 total）必须退回旧口径 spend，got %s", last.Kind)
	}

	// 快照已补上 total ⇒ 下一轮同类下滑才归因 expire
	l.DiffCredits("qodercn", "u5", 2300, shrinkItem(2300, 0, 2300))
	if last := lastEntry(t, l, dir); last.Kind != "expire" {
		t.Fatalf("补齐 total 后的池子缩水应记 expire，got %s", last.Kind)
	}
}

// TestDiffCreditsChannelFlagSharedAcrossAccounts 门槛 2 必须是**渠道级**判定。
//
// 真实漏洞（2026-10-19 修复过程中自查发现）：QoderCN 三个号里 019fe484 只签到不花钱，
// used 恒 0、total 2300 正好等于累计发放。若按「该 key 出现过 used>0」判，这个号会被
// 误归成「不区分 total/used 的渠道」，它的整包到期就仍会被记成消耗 —— 修了一半。
// 正确语义：「渠道会给 total 与 used 分别赋值」是**渠道属性**，任一账号任一 key 花过
// 一次就足以证明，全渠道共享。
func TestDiffCreditsChannelFlagSharedAcrossAccounts(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir)
	defer l.Close()

	// 账号 A：花过钱（used>0）⇒ 证实该渠道分别上报
	l.DiffCredits("qodercn", "spender", 2499, shrinkItem(2500, 1, 2499))
	// 账号 B：只签到，used 恒 0，两个包里都能平衡
	l.DiffCredits("qodercn", "idler", 2300, shrinkItem(2300, 0, 2300))
	// 账号 B 整包到期
	l.DiffCredits("qodercn", "idler", 2200, shrinkItem(2200, 0, 2200))

	last := lastEntry(t, l, dir)
	if last.Kind != "expire" {
		t.Fatalf("同渠道已有账号证实 used 记账，只签到号的整包到期也必须记 expire，got %s", last.Kind)
	}
	if last.Amount != -100 {
		t.Fatalf("amount=%d want -100", last.Amount)
	}
}

// TestDiffCreditsNoUsedChannelFlagNotPolluted 反向守门：真正不区分 total/used 的渠道
// 永远学不到「分别上报」的事实（used 恒 0），故它的消耗永远走 spend。
func TestDiffCreditsNoUsedChannelFlagNotPolluted(t *testing.T) {
	dir := t.TempDir()
	l, _ := New(dir)
	defer l.Close()

	wallets := func(remain int64) []provider.ResourceItem {
		return []provider.ResourceItem{
			{Name: "长期积分", Total: remain, Used: 0, Remain: remain, Key: "longterm", Usable: true},
		}
	}
	// 连刷三轮真实消耗（每轮 total 都跟着 remain 一起下降）
	for _, r := range []int64{2000, 1800, 1500} {
		l.DiffCredits("qwenwork", "w1", r, wallets(r))
	}
	l.Flush()
	if snap := l.loadSnapshots(); channelBooksUsage(snap, "qwenwork") {
		t.Fatal("used 恒 0 的渠道绝不能被标记成「分别上报」，否则真实消耗会被误记成过期")
	}
	if last := lastEntry(t, l, dir); last.Kind != "spend" {
		t.Fatalf("want spend got %s", last.Kind)
	}
}

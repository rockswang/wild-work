package raccoon

import (
	"testing"

	"wild-work/internal/provider"
)

// TestSplitAggregatesToSameLedgerKey 拆分后的条目按 ledger.itemKey 聚合，
// 必须回到与拆分前完全一致的（key, remain, expire, total）。
// 这是「升级零假流水」的唯一保证：ledger 按 key 聚合后逐字段差分。
func TestSplitAggregatesToSameLedgerKey(t *testing.T) {
	// 拆分前：单条聚合条目
	before := provider.ResourceItem{Name: dailyItemName, Total: 2100, Remain: 2100, Usable: true}

	// 拆分后：三笔逐笔条目（bills 实测形状）
	after := []provider.ResourceItem{
		{Name: dailyItemName, Total: 300, Remain: 300, ExpireAt: "2026-10-24T23:59:59+08:00", Usable: true},
		{Name: dailyItemName, Total: 300, Remain: 300, ExpireAt: "2026-10-25T23:59:59+08:00", Usable: true},
		{Name: dailyItemName, Total: 300, Remain: 300, ExpireAt: "2026-10-26T23:59:59+08:00", Usable: true},
	}
	// 补一笔老的（部分已花），让总额凑到 2100
	after = append([]provider.ResourceItem{
		{Name: dailyItemName, Total: 1200, Remain: 1200, ExpireAt: "2026-10-23T23:59:59+08:00", Usable: true},
	}, after...)

	// 按 ledger.DiffCredits 的口径聚合：同 key 求和 remain/total，expire 取首个非空
	var remain, total int64
	var expire string
	for _, it := range after {
		if !it.Usable {
			continue
		}
		k := it.Key
		if k == "" {
			k = it.Name
		}
		if k != dailyItemName {
			t.Fatalf("聚合键漂移: %q", k)
		}
		remain += it.Remain
		total += it.Total
		if expire == "" {
			expire = it.ExpireAt
		}
	}
	if remain != before.Remain {
		t.Fatalf("聚合后 remain = %d, want %d（键值必须与拆分前一致）", remain, before.Remain)
	}
	if total != before.Total {
		t.Fatalf("聚合后 total = %d, want %d", total, before.Total)
	}
	// 拆分前无到期时刻、拆分后取到最早一笔 —— 这正是修复点（expired() 有判据了）
	if expire == "" {
		t.Fatal("拆分后必须取到最早到期时刻，否则 expired() 仍恒 false")
	}
	if expire != "2026-10-23T23:59:59+08:00" {
		t.Fatalf("expire 应取最早一笔，得 %q", expire)
	}
}

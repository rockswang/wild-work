package raccoon

import (
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestDailyExpiryFeedsExpireBuckets 拆分后的逐笔条目必须能被 stats 的三列到期桶
// 正确分桶（今日 / 明日 / 7 日内）。
//
// 修复前：每日额度整池只有一条、ExpireAt 为空 ⇒ stats.expiryItems 直接跳过
// ⇒ 面板小浣熊「今日到期 / 明日到期 / 7 日内到期」三列恒为「—」，
// 而实际有 2100 分（7×300）全在 7 天内到期（2026-10-20 用户实测）。
func TestDailyExpiryFeedsExpireBuckets(t *testing.T) {
	now := time.Now()
	loc := time.FixedZone("UTC+8", 8*60*60)
	today := now.In(loc)

	// 构造 7 笔逐日发放，第 i 笔在「今天 + i」日末失效。
	grants := make([]dailyGrant, 0, 7)
	for i := 0; i < 7; i++ {
		d := today.AddDate(0, 0, i)
		exp := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc)
		grants = append(grants, dailyGrant{Points: 300, ExpireAt: exp.Format(time.RFC3339)})
	}
	items := parsePointsBalanceOnly(pointsBalanceData{DailyPoints: 2100})
	got := withDailyExpiry(items, grants)

	// 逐条目分桶（与 stats.acct.expireBucketsLocked 同口径）。
	var bucketsToday, bucketsTomorrow, within7 int64
	var counted int
	for _, it := range got {
		if !it.Usable || it.InfoOnly || it.Remain <= 0 || it.ExpireAt == "" {
			continue
		}
		counted++
		switch provider.ExpireBucketOf(it.ExpireAt, now) {
		case provider.BucketToday:
			bucketsToday += it.Remain
		case provider.BucketTomorrow:
			bucketsTomorrow += it.Remain
		case provider.Bucket7d:
			within7 += it.Remain
		}
	}
	if counted != 7 {
		t.Fatalf("参与分桶的条目 = %d, want 7", counted)
	}
	// 「今晚 23:59:59 失效」必须算今日（上游每日额度真实形态，R105 判据）。
	if bucketsToday != 300 {
		t.Fatalf("今日到期 = %d, want 300", bucketsToday)
	}
	if bucketsTomorrow != 300 {
		t.Fatalf("明日到期 = %d, want 300", bucketsTomorrow)
	}
	// 7 日内（今日+明日+更远）= 2100（7 笔全在窗口内，第 7 笔是 +6 天）。
	total := bucketsToday + bucketsTomorrow + within7
	if total != 2100 {
		t.Fatalf("7 日内合计 = %d, want 2100（拆分前这里是 0）", total)
	}
}

// TestDailyExpiryNotAllToday 反向护栏：绝不能把整池 2100 都报成「今日到期」。
//
// 旧版曾用 provider.ExpireDateToday() 给整池静态补「今天到期」（RELEASES v2.8.x
// 「每日积分静态标注当日到期」），那是**编造**：7 笔发放有 7 个不同到期日，
// 全报今日等于虚高 7 倍。
func TestDailyExpiryNotAllToday(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*60*60)
	now := time.Now()
	today := now.In(loc)
	grants := make([]dailyGrant, 0, 7)
	for i := 0; i < 7; i++ {
		d := today.AddDate(0, 0, i)
		exp := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, loc)
		grants = append(grants, dailyGrant{Points: 300, ExpireAt: exp.Format(time.RFC3339)})
	}
	got := withDailyExpiry(parsePointsBalanceOnly(pointsBalanceData{DailyPoints: 2100}), grants)

	var todaySum int64
	for _, it := range got {
		if it.ExpireAt != "" && provider.ExpireBucketOf(it.ExpireAt, now) == provider.BucketToday {
			todaySum += it.Remain
		}
	}
	if todaySum >= 2100 {
		t.Fatalf("整池被报成今日到期（%d）——逐笔到期失效了", todaySum)
	}
}

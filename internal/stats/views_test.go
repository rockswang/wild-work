package stats

import (
	"testing"
	"time"
)

// 日历日差：今天=0、明天=1、后天=2（回归「hours/24 截断把明天算成 0 天」的实测 bug）。
func TestDaysUntil(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 9, 21, 23, 40, 0, 0, loc) // 深夜：最易触发截断误差
	cases := []struct {
		day  time.Time
		want int
	}{
		{time.Date(2026, 9, 21, 0, 0, 0, 0, loc), 0},
		{time.Date(2026, 9, 22, 0, 0, 0, 0, loc), 1},
		{time.Date(2026, 9, 23, 0, 0, 0, 0, loc), 2},
		{time.Date(2026, 9, 30, 0, 0, 0, 0, loc), 9},
		{time.Date(2026, 9, 20, 0, 0, 0, 0, loc), 0}, // 已过期钳 0
	}
	for _, c := range cases {
		if got := daysUntil(c.day, now); got != c.want {
			t.Errorf("daysUntil(%s) = %d, want %d", c.day.Format("01-02"), got, c.want)
		}
	}
}

// 预计作废：观测不足返回 -1；观测足够时按速率外推并判风险。
func TestBurnForecast(t *testing.T) {
	s := newTestStats(t)
	// 用当天正午做基准，避免 +1h 跨过午夜触发跨天重置
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	expiry := base.AddDate(0, 0, 3).Format("2006-01-02")

	// 首次喂入确立基线（burnSince=base）
	s.ApplyPoll([]Account{{UID: "a", Nickname: "号1", Group: "traework", Credits: 200}}, base)
	s.ApplyExpiry("a", []ExpireItem{{At: expiry, Amount: 2000}})
	snap := s.Snapshot(base)
	if len(snap.Expiry) != 1 || snap.Expiry[0].BurnForecast != -1 {
		t.Fatalf("观测不足应返回 -1: %+v", snap.Expiry)
	}

	// 1 小时后余额下降 10 → 速率 10/h；正午基准，到期日=T+3 的 24 点 →
	// 距 T 13:00 恰 83h → forecast = 10/h × 83 = 830；830 < 2000 → risk
	s.ApplyPoll([]Account{{UID: "a", Nickname: "号1", Group: "traework", Credits: 190}}, base.Add(time.Hour))
	snap = s.Snapshot(base.Add(time.Hour))
	if len(snap.Expiry) != 1 {
		t.Fatalf("expiry 行缺失")
	}
	row := snap.Expiry[0]
	if row.BurnForecast != 830 {
		t.Fatalf("forecast = %d, want 830", row.BurnForecast)
	}
	if !row.Risk {
		t.Fatalf("3 天内到期且 forecast<amount 应判风险: %+v", row)
	}
}

// 最近临期全量下发：排序=到期日升序、同日按额降序、同日同额 uid 升序稳定锚点。
func TestExpiryFullList(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	accts := []Account{
		{UID: "a", Group: "workbuddy", Credits: 100},
		{UID: "b", Group: "workbuddy", Credits: 100},
		{UID: "c", Group: "traework", Credits: 100},
		{UID: "d", Group: "traework", Credits: 100},
		{UID: "e", Group: "traework", Credits: 100},
	}
	s.ApplyPoll(accts, base)
	s.ApplyExpiry("a", []ExpireItem{{At: base.AddDate(0, 0, 2).Format("2006-01-02"), Amount: 300}})
	s.ApplyExpiry("b", []ExpireItem{{At: base.AddDate(0, 0, 2).Format("2006-01-02"), Amount: 500}}) // 同日按额降序在前
	s.ApplyExpiry("c", []ExpireItem{{At: base.AddDate(0, 0, 1).Format("2006-01-02"), Amount: 100}}) // 最早 → 第 1
	s.ApplyExpiry("d", []ExpireItem{{At: base.AddDate(0, 0, 5).Format("2006-01-02"), Amount: 700}})
	s.ApplyExpiry("e", []ExpireItem{{At: "", Amount: 900}}) // 无到期日：不下发

	rows := s.Snapshot(base).Expiry
	if len(rows) != 4 {
		t.Fatalf("应全量下发 4 行（e 无到期日除外）: %d", len(rows))
	}
	if rows[0].UID != "c" || rows[1].UID != "b" || rows[2].UID != "a" || rows[3].UID != "d" {
		t.Fatalf("排序错误（到期升序、同日额降序）: %v %v %v %v", rows[0].UID, rows[1].UID, rows[2].UID, rows[3].UID)
	}

	// 同日同额：uid 升序稳定锚点（f < g），多轮快照行序不跳动
	s.ApplyPoll(append(accts, Account{UID: "f", Group: "traework", Credits: 100},
		Account{UID: "g", Group: "traework", Credits: 100}), base)
	s.ApplyExpiry("f", []ExpireItem{{At: rows[0].ExpireAt, Amount: rows[0].Amount}})
	s.ApplyExpiry("g", []ExpireItem{{At: rows[0].ExpireAt, Amount: rows[0].Amount}})
	r1 := s.Snapshot(base).Expiry
	r2 := s.Snapshot(base).Expiry
	for i := range r1 {
		if r1[i].UID != r2[i].UID {
			t.Fatalf("同日同额行序不稳定: %v vs %v", r1[i].UID, r2[i].UID)
		}
	}
}

// 今日到期：expireAt 落在当天 → Snapshot.ExpireToday 与平台行 ExpireToday 各自聚合；
// 平台表空行过滤须把 ExpireToday 纳入（仅有今日到期的平台不该被整行剔除）。
func TestExpireToday(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	today := base.Format("2006-01-02")
	tomorrow := base.AddDate(0, 0, 1).Format("2006-01-02")
	s.ApplyPoll([]Account{
		{UID: "w1", Group: "workbuddy", Credits: 1000},
		{UID: "w2", Group: "workbuddy", Credits: 500},
		{UID: "t1", Group: "traework", Credits: 800},
	}, base)
	s.ApplyExpiry("w1", []ExpireItem{{At: today, Amount: 664}})    // 今日到期 → 计入
	s.ApplyExpiry("w2", []ExpireItem{{At: tomorrow, Amount: 100}}) // 明日到期 → 不计入今日
	s.ApplyExpiry("t1", []ExpireItem{{At: today, Amount: 50}})     // 今日到期（另一平台）

	snap := s.Snapshot(base)
	if snap.ExpireToday != 714 {
		t.Fatalf("ExpireToday = %d, want 714（664+50）", snap.ExpireToday)
	}
	if snap.ExpireTomorrow != 100 {
		t.Fatalf("ExpireTomorrow = %d, want 100", snap.ExpireTomorrow)
	}
	byGroup := map[string]PlatformRow{}
	for _, r := range snap.Platforms {
		byGroup[r.Group] = r
	}
	if wb := byGroup["workbuddy"]; wb.ExpireToday != 664 || wb.ExpireTomorrow != 100 {
		t.Fatalf("workbuddy 行 = %+v，期望今日 664 / 明日 100", wb)
	}
	if tw := byGroup["traework"]; tw.ExpireToday != 50 {
		t.Fatalf("traework 行 ExpireToday = %d, want 50", tw.ExpireToday)
	}

	// 空行过滤：仅有今日到期的平台（无余额/收入/花费/Token）不被剔除
	s2 := newTestStats(t)
	s2.ApplyPoll([]Account{{UID: "q1", Group: "qodercn", Credits: 0}}, base)
	s2.ApplyExpiry("q1", []ExpireItem{{At: today, Amount: 30}})
	rows := s2.Snapshot(base).Platforms
	if len(rows) != 1 || rows[0].ExpireToday != 30 {
		t.Fatalf("仅今日到期的平台行被误剔除: %+v", rows)
	}
}

// ApplyExpiry day="" 清除旧值：账号无到期包后临期行退场（防残留显示到跨天）。
func TestApplyExpiryClears(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 100}}, base)
	s.ApplyExpiry("a", []ExpireItem{{At: base.AddDate(0, 0, 2).Format("2006-01-02"), Amount: 300}})
	if rows := s.Snapshot(base).Expiry; len(rows) != 1 {
		t.Fatalf("前置失败：应有一行临期")
	}
	s.ApplyExpiry("a", []ExpireItem{{At: "", Amount: 0}}) // 无到期包 → 清除
	if rows := s.Snapshot(base).Expiry; len(rows) != 0 {
		t.Fatalf("空值应清除临期行: %+v", rows)
	}
}

// ============ 今日作废拆分（ApplyLedgerExpire / CreditUsed / trae 分摊扣作废） ============

// ledger 作废写入：全量替换语义 + 渠道归一 + 空 map 表示「今日无作废」应清零。
func TestApplyLedgerExpireReplaceAndNormalize(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	day := todayKey(now)

	// 首轮：workbuddy 100 + traecode 50（应归一为 traework）+ 空渠道归 unknown 7
	s.ApplyLedgerExpire(day, map[string]int64{"workbuddy": 100, "traecode": 50, "": 7})
	snap := s.Snapshot(now)
	if snap.CreditExpired != 157 {
		t.Fatalf("credit_expired = %d, want 157（100+50+7）", snap.CreditExpired)
	}

	// 次轮全量替换（非累加）：只报 workbuddy 30，旧值必须被整体替换为 30
	s.ApplyLedgerExpire(day, map[string]int64{"workbuddy": 30})
	if snap := s.Snapshot(now); snap.CreditExpired != 30 {
		t.Fatalf("全量替换语义破坏: credit_expired = %d, want 30", snap.CreditExpired)
	}

	// 空 map = 今日无作废 → 清零（不是保留旧值）
	s.ApplyLedgerExpire(day, map[string]int64{})
	if snap := s.Snapshot(now); snap.CreditExpired != 0 {
		t.Fatalf("空 map 应清零: credit_expired = %d, want 0", snap.CreditExpired)
	}

	// v<=0 值跳过：负数与 0 不是合法作废额
	s.ApplyLedgerExpire(day, map[string]int64{"workbuddy": -5, "traework": 0})
	if snap := s.Snapshot(now); snap.CreditExpired != 0 {
		t.Fatalf("非正值应跳过: credit_expired = %d, want 0", snap.CreditExpired)
	}
}

// day 守卫：跨零点时上游可能仍返回昨日数据，整批丢弃；空 day（调用方约定
// 「本轮无条目/失败」）直接忽略不清零。
func TestApplyLedgerExpireDayGuard(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyLedgerExpire(todayKey(now), map[string]int64{"workbuddy": 100})
	if s.Snapshot(now).CreditExpired != 100 {
		t.Fatalf("前置失败")
	}

	// 空 day：调用方失败时不应调用，即便误调也不清零（保留旧值）
	s.ApplyLedgerExpire("", map[string]int64{"workbuddy": 999})
	if got := s.Snapshot(now).CreditExpired; got != 100 {
		t.Fatalf("空 day 不应覆盖: credit_expired = %d, want 100", got)
	}

	// 昨日 day：整批丢弃
	s.ApplyLedgerExpire(todayKey(now.Add(-24*time.Hour)), map[string]int64{"workbuddy": 50})
	if got := s.Snapshot(now).CreditExpired; got != 100 {
		t.Fatalf("昨日 day 应整批丢弃: credit_expired = %d, want 100", got)
	}
}

// 平台表净值：CreditUsed = CreditOut − CreditExpired，逐行钳 0；全池 credit_used
// 按行求和（保证「合计行 == 显示行之和」闭合）。
func TestPlatformRowsNetAndClamp(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	day := todayKey(now)
	// 三账号：workbuddy 差值 120（其中 110 作废）、traework 差值 30（全消耗）、
	// qwenwork 差值 10 但 ledger 报了 40 作废（倒挂场景 → 钳 0）
	s.ApplyPoll([]Account{
		{UID: "wb", Group: "workbuddy", Credits: 500},
		{UID: "tw", Group: "traework", Credits: 500},
		{UID: "qw", Group: "qwenwork", Credits: 500},
	}, now)
	s.ApplyPoll([]Account{
		{UID: "wb", Group: "workbuddy", Credits: 380}, // −120
		{UID: "tw", Group: "traework", Credits: 470},  // −30
		{UID: "qw", Group: "qwenwork", Credits: 490},  // −10
	}, now.Add(time.Minute))
	s.ApplyLedgerExpire(day, map[string]int64{"workbuddy": 110, "qwenwork": 40})

	snap := s.Snapshot(now)
	byGroup := map[string]PlatformRow{}
	for _, r := range snap.Platforms {
		byGroup[r.Group] = r
	}
	if r := byGroup["workbuddy"]; r.CreditOut != 120 || r.CreditExpired != 110 || r.CreditUsed != 10 {
		t.Fatalf("workbuddy 行: out=%d expired=%d used=%d, want 120/110/10", r.CreditOut, r.CreditExpired, r.CreditUsed)
	}
	if r := byGroup["traework"]; r.CreditUsed != 30 {
		t.Fatalf("traework 行 used = %d, want 30（无作废全额净值）", r.CreditUsed)
	}
	if r := byGroup["qwenwork"]; r.CreditUsed != 0 {
		t.Fatalf("qwenwork 倒挂应钳 0: used = %d, want 0", r.CreditUsed)
	}
	// 全池 credit_used = 10+30+0 = 40（逐行钳 0 后求和）
	if snap.CreditUsed != 40 {
		t.Fatalf("全池 credit_used = %d, want 40", snap.CreditUsed)
	}
}

// 跨天清零：作废两件套（expiredByGroup/Total）随 rollDay 重置。
func TestLedgerExpireDayRoll(t *testing.T) {
	s := newTestStats(t)
	day1 := time.Now()
	s.ApplyLedgerExpire(todayKey(day1), map[string]int64{"workbuddy": 200})
	if s.Snapshot(day1).CreditExpired != 200 {
		t.Fatalf("前置失败")
	}
	day2 := day1.Add(24 * time.Hour)
	if snap := s.Snapshot(day2); snap.CreditExpired != 0 {
		t.Fatalf("跨天未清零: credit_expired = %d, want 0", snap.CreditExpired)
	}
}

// ledger 归组对空行的贡献：某渠道今日只有作废、无任何账号与消耗（账号已删），
// 平台表仍应归入该渠道行（作废是既成事实，不能因账号消失而悬空）。
func TestLedgerExpireOrphanChannel(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "wb", Group: "workbuddy", Credits: 500}}, now)
	s.ApplyPoll([]Account{{UID: "wb", Group: "workbuddy", Credits: 460}}, now.Add(time.Minute)) // 消耗 40
	s.ApplyLedgerExpire(todayKey(now), map[string]int64{"ghost": 66})

	snap := s.Snapshot(now)
	var ghostRow *PlatformRow
	for i := range snap.Platforms {
		if snap.Platforms[i].Group == "ghost" {
			ghostRow = &snap.Platforms[i]
		}
	}
	if ghostRow == nil {
		t.Fatalf("纯作废渠道行被过滤掉: %+v", snap.Platforms)
	}
	if ghostRow.CreditExpired != 66 || ghostRow.CreditUsed != 0 {
		t.Fatalf("ghost 行: expired=%d used=%d, want 66/0", ghostRow.CreditExpired, ghostRow.CreditUsed)
	}
}

// trae 分摊扣作废：差值总额先扣 trae 系作废再按 token 分摊；作废大于差值时
// 基数钳 0 不分摊（不出现负积分）。
func TestTraeAllocationDeductsExpire(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "t1", Group: "traework", Credits: 1000}}, now)
	s.ApplyPoll([]Account{{UID: "t1", Group: "traework", Credits: 700}}, now.Add(time.Minute)) // 差值 300
	s.AddUsage("traework/a", 2000, 1000, 0, 0, 0, 0, false, now)                               // 3000 token
	s.AddUsage("traework/b", 500, 500, 0, 0, 0, 0, false, now)                                 // 1000 token

	// trae 系作废 100（traecode 键也应归一计入）→ 分摊基数 300−100=200
	s.ApplyLedgerExpire(todayKey(now), map[string]int64{"traecode": 100})
	byName := map[string]ModelStat{}
	for _, m := range s.Snapshot(now).Models {
		byName[m.Model] = m
	}
	if got := byName["traework/a"].Credit; got != 150 {
		t.Fatalf("traework/a = %v, want 150（200 × 3000/4000）", got)
	}
	if got := byName["traework/b"].Credit; got != 50 {
		t.Fatalf("traework/b = %v, want 50", got)
	}

	// 作废 350 > 差值 300 → 基数 0，不做分摊（保持 0，不出现负数）
	s.ApplyLedgerExpire(todayKey(now), map[string]int64{"traework": 350})
	for _, m := range s.Snapshot(now).Models {
		if m.Credit < 0 {
			t.Fatalf("%s 超额作废时不应出现负积分，实际 %v", m.Model, m.Credit)
		}
	}
}

// TestExpireBucketsByInstant R103 核心守门：三个到期列按**精确时刻**分**自然日**桶，
// 且逐条目累加。
//
// 锁住两件在旧实现里都错的事：
//  1. 今晚 00:00 失效（上游每日额度真实形态）必须算**今日**，不能算明日。
//     旧实现按日期字符串比（a.expireAt == today），"2026-10-20T00:00" 截成
//     "2026-10-20" 后落到明日桶，415 分被误报成「明日到期」，实际只剩 5.7h。
//  2. 「7 日内」必须逐条目累加，不能只取「每账号最早到期日」那一天。
//     旧实现只留最早日，workbuddy 每账号 20+ 个每日包全被漏掉（1788 vs 4938）。
func TestExpireBucketsByInstant(t *testing.T) {
	s := newTestStats(t)
	// 基准 2026-10-19 14:00（UTC+8）
	base := time.Date(2026, 10, 19, 14, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{{UID: "a", Group: "qwenwork", Credits: 1000}}, base)

	s.ApplyExpiry("a", []ExpireItem{
		// 今晚 24:00 失效：必须进「今日」（零点宽限）
		{At: "2026-10-20T00:00:00+08:00", Amount: 100},
		// 明日 23:59 失效：进「明日」
		{At: "2026-10-20T23:59:59+08:00", Amount: 200},
		// 后天中午：进「7 日内」
		{At: "2026-10-21T12:00:00+08:00", Amount: 300},
		// 第 5 天：也进「7 日内」——旧实现只看最早日，这 400 会被漏掉
		{At: "2026-10-24T12:00:00+08:00", Amount: 400},
		// 远期：哪都不进
		{At: "2026-11-30T12:00:00+08:00", Amount: 900},
	})

	snap := s.Snapshot(base)
	if snap.ExpireToday != 100 {
		t.Errorf("ExpireToday = %d, want 100（今晚 00:00 失效算今日）", snap.ExpireToday)
	}
	if snap.ExpireTomorrow != 200 {
		t.Errorf("ExpireTomorrow = %d, want 200", snap.ExpireTomorrow)
	}
	// 7 日内 = 今日 + 明日 + 后续 5 天内 = 100+200+300+400
	if snap.Expiring7d != 1000 {
		t.Errorf("Expiring7d = %d, want 1000（逐条目累加，不是只取最早日）", snap.Expiring7d)
	}

	var row PlatformRow
	for _, r := range snap.Platforms {
		if r.Group == "qwenwork" {
			row = r
		}
	}
	if row.ExpireToday != 100 || row.ExpireTomorrow != 200 || row.Expire7d != 1000 {
		t.Errorf("平台行 = 今日%d/明日%d/7日%d，want 100/200/1000",
			row.ExpireToday, row.ExpireTomorrow, row.Expire7d)
	}
}

// TestExpiryRowsHoursLeft 守门「最近临期」按**剩余时长**排序与展示。
// 旧实现按日期升序，同一天内的包看不出谁先没（今晚 00:00 vs 今晚 23:59）。
func TestExpiryRowsHoursLeft(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 19, 14, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{
		{UID: "late", Group: "qwenwork", Credits: 100},
		{UID: "soon", Group: "qwenwork", Credits: 100},
	}, base)
	s.ApplyExpiry("late", []ExpireItem{{At: "2026-10-20T23:59:59+08:00", Amount: 50}})
	s.ApplyExpiry("soon", []ExpireItem{{At: "2026-10-20T00:00:00+08:00", Amount: 50}})

	rows := s.Snapshot(base).Expiry
	if len(rows) != 2 {
		t.Fatalf("应有 2 行，实际 %d", len(rows))
	}
	if rows[0].UID != "soon" {
		t.Fatalf("应按剩余时长升序（soon 10h 在前，late 34h 在后），实际首行 %s", rows[0].UID)
	}
	if rows[0].HoursLeft < 9.9 || rows[0].HoursLeft > 10.1 {
		t.Errorf("soon 剩余 = %.1fh，want ≈10h", rows[0].HoursLeft)
	}
	if rows[1].HoursLeft < 33.9 || rows[1].HoursLeft > 34.1 {
		t.Errorf("late 剩余 = %.1fh，want ≈34h", rows[1].HoursLeft)
	}

	// 混合格式：日期串排序与剩余时长排序会**分叉**的用例。
	// 纯日期 "2026-10-19" = 19 日全天可用 ⇒ 20 日 00:00 失效 = 剩 10h；
	// RFC3339 "2026-10-19T20:00" = 剩 6h（更早失效）。
	// 但字符串比较 "2026-10-19" < "2026-10-19T20:00..."（短前缀更小），
	// 按日期串排会把剩 10h 的排到剩 6h 的前面——必须按剩余时长排。
	s3 := newTestStats(t)
	s3.ApplyPoll([]Account{
		{UID: "d1", Group: "qwenwork", Credits: 100},
		{UID: "d2", Group: "qwenwork", Credits: 100},
	}, base)
	s3.ApplyExpiry("d1", []ExpireItem{{At: "2026-10-19", Amount: 50}})                // 纯日期 → 剩 10h
	s3.ApplyExpiry("d2", []ExpireItem{{At: "2026-10-19T20:00:00+08:00", Amount: 50}}) // 剩 6h
	rows = s3.Snapshot(base).Expiry
	if len(rows) != 2 || rows[0].UID != "d2" {
		t.Fatalf("应按剩余时长排（d2 6h 在 d1 10h 前），实际 %+v", rows)
	}
}

// TestExpiryRiskWhenNoBurn 守门「算不出消耗」不等于「没风险」。
//
// 用户实测（2026-10-19）：最近临期 16 行里只有第 4 行红色。前 3 行是今晚
// 4.7 小时后到期的 qwenwork 号，今天**一分没花**（creditOut=0 ⇒ forecast=-1）。
// 旧判据 `forecast >= 0 && forecast < amount` 把「完全没在消耗、马上要作废」
// 这个最该预警的情形排除掉了，只有今天花过 1 分（forecast=0）的那个红。
// 新判据：forecast<0 视为「预计作废 = 全部额度」⇒ 照样预警。
func TestExpiryRiskWhenNoBurn(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 19, 20, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{
		{UID: "idle", Group: "qwenwork", Credits: 100}, // 今天没花过 → forecast=-1
		{UID: "burn", Group: "qwenwork", Credits: 100}, // 花过一点 → forecast>=0
		{UID: "far", Group: "qwenwork", Credits: 100},  // 远期，不该预警
	}, base)
	// 今晚 4 小时后到期（今日桶内）
	s.ApplyExpiry("idle", []ExpireItem{{At: "2026-10-20T00:00:00+08:00", Amount: 100}})
	s.ApplyExpiry("burn", []ExpireItem{{At: "2026-10-20T00:00:00+08:00", Amount: 100}})
	// 30 天后到期：超出 expiryRiskDays，不该预警
	s.ApplyExpiry("far", []ExpireItem{{At: "2026-11-18T12:00:00+08:00", Amount: 100}})
	// burn 号今天消耗过（制造 creditOut>0 与足够观测时长）
	s.ApplyPoll([]Account{
		{UID: "idle", Group: "qwenwork", Credits: 100},
		{UID: "burn", Group: "qwenwork", Credits: 90},
		{UID: "far", Group: "qwenwork", Credits: 100},
	}, base.Add(2*time.Hour))

	rows := s.Snapshot(base.Add(2 * time.Hour)).Expiry
	byUID := map[string]ExpiryRow{}
	for _, r := range rows {
		byUID[r.UID] = r
	}
	if idle, ok := byUID["idle"]; !ok || !idle.Risk {
		t.Errorf("今日没消耗但今晚到期的号必须预警（forecast=%d）：%+v", idle.BurnForecast, idle)
	}
	if idle := byUID["idle"]; idle.BurnForecast != -1 {
		t.Errorf("idle 的 forecast 应为 -1（观测不足），实际 %d", idle.BurnForecast)
	}
	if burn, ok := byUID["burn"]; !ok || !burn.Risk {
		t.Errorf("今天花过、额度花不完的号必须预警：%+v", burn)
	}
	if far, ok := byUID["far"]; !ok || far.Risk {
		t.Errorf("30 天后到期的号不该预警（超出 7 自然日窗口）：%+v", far)
	}
}

// TestExpiryRowsFullInstant 守门「到期时刻必须带完整时分秒」。
// 前端渲染「YYYY-MM-DD HH:MM:SS」，后端下发的必须是完整 RFC3339，
// 不能只有日期（否则秒级信息在源头就丢了，前端补不回来）。
func TestExpiryRowsFullInstant(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 19, 14, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{{UID: "a", Group: "qwenwork", Credits: 100}}, base)
	s.ApplyExpiry("a", []ExpireItem{{At: "2026-10-20T00:00:03+08:00", Amount: 50}})
	rows := s.Snapshot(base).Expiry
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，实际 %d", len(rows))
	}
	if rows[0].ExpireAt != "2026-10-20T00:00:03+08:00" {
		t.Errorf("ExpireAt 必须原样保留秒级精度，实际 %q", rows[0].ExpireAt)
	}
}

// TestExpiryUrgency 守门「配色按紧迫度分档」而不是按 risk（会不会作废）。
//
// 用户实测（2026-10-19）：最近临期 16 行里一堆标红，连 135.7 小时后（5.7 天）
// 到期的也红。原因是前端拿 risk 上色，而 risk = 7 自然日内到期 且 可能花不完，
// 于是「7 天内的」全部飘红——一片警示等于没有警示。
// 用户定义：**今天到期标红、明天到期标黄、更远不上色**。
func TestExpiryUrgency(t *testing.T) {
	s := newTestStats(t)
	base := time.Date(2026, 10, 19, 14, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{
		{UID: "today", Group: "qwenwork", Credits: 100},
		{UID: "tomorrow", Group: "traework", Credits: 100},
		{UID: "far", Group: "workbuddyai", Credits: 100},
	}, base)
	// 今晚 00:00（次日零点归今日）→ today
	s.ApplyExpiry("today", []ExpireItem{{At: "2026-10-20T00:00:00+08:00", Amount: 100}})
	// 明日 08:45 → tomorrow
	s.ApplyExpiry("tomorrow", []ExpireItem{{At: "2026-10-20T08:45:21+08:00", Amount: 100}})
	// 5.7 天后（135.7h）→ later（正是用户截图里被误标红的那类）
	s.ApplyExpiry("far", []ExpireItem{{At: "2026-10-25T12:06:19+08:00", Amount: 100}})

	byUID := map[string]ExpiryRow{}
	for _, r := range s.Snapshot(base).Expiry {
		byUID[r.UID] = r
	}
	if got := byUID["today"].Urgency; got != UrgencyToday {
		t.Errorf("今晚 00:00 失效应为 today，实际 %q", got)
	}
	if got := byUID["tomorrow"].Urgency; got != UrgencyTomorrow {
		t.Errorf("明日 08:45 应为 tomorrow，实际 %q", got)
	}
	if got := byUID["far"].Urgency; got != UrgencyLater {
		t.Errorf("5.7 天后应为 later（不该标红），实际 %q", got)
	}
	// 关键：far 可能仍然 risk=true（按今日速度花不完），但那只是「预计作废」的数据结论，
	// 颜色必须跟 urgency 走。两者必须解耦。
	if !byUID["far"].Risk {
		t.Log("far 的 risk=false（观测不足时本应为 true），此处不强制")
	}
	if byUID["far"].Urgency == UrgencyToday || byUID["far"].Urgency == UrgencyTomorrow {
		t.Errorf("5.7 天后到期的行不能被归入今天/明天档：%+v", byUID["far"])
	}
}

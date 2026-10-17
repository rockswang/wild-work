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
	s.ApplyExpiry("a", expiry, 2000)
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
	s.ApplyExpiry("a", base.AddDate(0, 0, 2).Format("2006-01-02"), 300)
	s.ApplyExpiry("b", base.AddDate(0, 0, 2).Format("2006-01-02"), 500) // 同日按额降序在前
	s.ApplyExpiry("c", base.AddDate(0, 0, 1).Format("2006-01-02"), 100) // 最早 → 第 1
	s.ApplyExpiry("d", base.AddDate(0, 0, 5).Format("2006-01-02"), 700)
	s.ApplyExpiry("e", "", 900) // 无到期日：不下发

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
	s.ApplyExpiry("f", rows[0].ExpireAt, rows[0].Amount)
	s.ApplyExpiry("g", rows[0].ExpireAt, rows[0].Amount)
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
	s.ApplyExpiry("w1", today, 664)    // 今日到期 → 计入
	s.ApplyExpiry("w2", tomorrow, 100) // 明日到期 → 不计入今日
	s.ApplyExpiry("t1", today, 50)     // 今日到期（另一平台）

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
	s2.ApplyExpiry("q1", today, 30)
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
	s.ApplyExpiry("a", base.AddDate(0, 0, 2).Format("2006-01-02"), 300)
	if rows := s.Snapshot(base).Expiry; len(rows) != 1 {
		t.Fatalf("前置失败：应有一行临期")
	}
	s.ApplyExpiry("a", "", 0) // 无到期包 → 清除
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

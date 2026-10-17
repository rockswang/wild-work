package stats

import (
	"os"
	"strings"
	"testing"
	"time"
)

// tNow 固定时区的时间锚（测试跨天用）。
func tNow(day int, hour int) time.Time {
	return time.Date(2026, 10, day, hour, 0, 0, 0, time.Local)
}

func acct1(credits int64) Account {
	return Account{UID: "u1", Group: "workbuddy", Nickname: "n1", Credits: credits}
}

func newTestStats(t *testing.T) *Stats {
	t.Helper()
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestApplyPollBaselineOnly(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{acct1(1000)}, tNow(1, 9))
	snap := s.Snapshot(tNow(1, 9))
	if snap.CreditOut != 0 {
		t.Fatalf("首轮只建基线，不应记账: got %d", snap.CreditOut)
	}
	if snap.TotalCredits != 1000 || snap.TotalAccounts != 1 {
		t.Fatalf("余额/账号数错误: credits=%d accounts=%d", snap.TotalCredits, snap.TotalAccounts)
	}
}

func TestApplyPollSpendOnlyOnDrop(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{acct1(1000)}, tNow(1, 9))
	// 下降 300 → 记 300；再上涨 500 → 不计收入（事件口径）
	s.ApplyPoll([]Account{acct1(700)}, tNow(1, 10))
	s.ApplyPoll([]Account{acct1(1200)}, tNow(1, 11))
	snap := s.Snapshot(tNow(1, 11))
	if snap.CreditOut != 300 {
		t.Fatalf("余额上涨不应计花费: got %d want 300", snap.CreditOut)
	}
	if snap.CreditIn != 0 {
		t.Fatalf("余额上涨不应记收入（事件口径）: credit_in = %d", snap.CreditIn)
	}
	if snap.TotalCredits != 1200 {
		t.Fatalf("当前余额应为最新快照值: got %d", snap.TotalCredits)
	}
}

func TestApplyPollEmptySkipped(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{acct1(1000)}, tNow(1, 9))
	s.ApplyPoll([]Account{acct1(600)}, tNow(1, 10)) // 已记 400
	s.ApplyPoll(nil, tNow(1, 11))                   // 空快照：跳过，不清零
	snap := s.Snapshot(tNow(1, 11))
	if snap.CreditOut != 400 || snap.TotalAccounts != 1 {
		t.Fatalf("空快照应跳过: out=%d accounts=%d", snap.CreditOut, snap.TotalAccounts)
	}
}

func TestApplyPollDeletedAccount(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{acct1(1000), {UID: "u2", Group: "traework", Credits: 500}}, tNow(1, 9))
	// u2 消失 → 剔除（防已删号差值虚高）
	s.ApplyPoll([]Account{acct1(300)}, tNow(1, 10))
	snap := s.Snapshot(tNow(1, 10))
	if snap.TotalAccounts != 1 {
		t.Fatalf("已删号应剔除: got %d", snap.TotalAccounts)
	}
	if len(snap.Platforms) != 1 || snap.Platforms[0].Group != "workbuddy" {
		t.Fatalf("平台行应只剩 workbuddy: %+v", snap.Platforms)
	}
}

// 匿名渠道（CreditsNA）：计入账号数、不计余额合计；其平台行全数值为零会被
// 空行过滤剔除（对齐「过滤空平台」口径：有号但无余额无流量的渠道不占版面）。
func TestApplyPollCreditsNAExcludedFromTotal(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{
		acct1(1000),
		{UID: "oc", Group: "oczen", Nickname: "匿名", Credits: 0, CreditsNA: true},
	}, tNow(1, 9))
	snap := s.Snapshot(tNow(1, 9))
	if snap.TotalAccounts != 2 {
		t.Fatalf("匿名号计入账号数: got %d", snap.TotalAccounts)
	}
	if snap.TotalCredits != 1000 {
		t.Fatalf("匿名号不计余额合计: got %d", snap.TotalCredits)
	}
	for _, r := range snap.Platforms {
		if r.Group == "oczen" {
			t.Fatalf("全零平台行应被空行过滤剔除: %+v", snap.Platforms)
		}
	}
	if len(snap.Platforms) != 1 || snap.Platforms[0].Group != "workbuddy" {
		t.Fatalf("应只剩 workbuddy 平台行: %+v", snap.Platforms)
	}
	// 匿名账号余额波动不入差值口径（CreditsNA 差值门禁）
	s.ApplyPoll([]Account{
		acct1(1000),
		{UID: "oc", Group: "oczen", Nickname: "匿名", Credits: 50, CreditsNA: true},
	}, tNow(1, 10))
	s.ApplyPoll([]Account{
		acct1(1000),
		{UID: "oc", Group: "oczen", Nickname: "匿名", Credits: 10, CreditsNA: true},
	}, tNow(1, 11))
	if snap := s.Snapshot(tNow(1, 11)); snap.CreditOut != 0 {
		t.Fatalf("匿名账号余额下降不应入差值口径: got %d", snap.CreditOut)
	}
}

func TestRollDayResets(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{acct1(1000)}, tNow(1, 9))
	s.ApplyPoll([]Account{acct1(100)}, tNow(1, 10)) // 记 900，触发 9 个里程碑（合并 1 条 Toast）
	snap1 := s.Snapshot(tNow(1, 10))
	if snap1.CreditOut != 900 || len(snap1.ToastQueue) != 1 {
		t.Fatalf("跨天前应有花费与 Toast: out=%d toasts=%d", snap1.CreditOut, len(snap1.ToastQueue))
	}
	// 次日首轮：清零 + 基线延续（sidecar 语义）
	s.ApplyPoll([]Account{acct1(100)}, tNow(2, 9))
	snap2 := s.Snapshot(tNow(2, 9))
	if snap2.CreditOut != 0 {
		t.Fatalf("跨天应清零花费: got %d", snap2.CreditOut)
	}
	if snap2.Today != "2026-10-02" {
		t.Fatalf("日键应滚动: got %s", snap2.Today)
	}
	if len(snap2.ToastQueue) != 0 {
		t.Fatalf("跨天应清空 Toast: got %d", len(snap2.ToastQueue))
	}
	if snap2.MilestoneCount != 0 {
		t.Fatalf("跨天应清零里程碑: got %d", snap2.MilestoneCount)
	}
	// 跨零点差值归因（sidecar 语义锚定）：次日余额下降的差值记入新一天
	//（宁可归因到新一天也不让总量凭空蒸发；注释见 rollDayLocked「已知边界」）。
	s.ApplyPoll([]Account{acct1(50)}, tNow(2, 10))
	if snap := s.Snapshot(tNow(2, 10)); snap.CreditOut != 50 {
		t.Fatalf("跨零点差值应记入新一天: got %d want 50", snap.CreditOut)
	}
}

// 跨天归档：今日 tokens/reqs/模型消耗转入历史 map，全部今日口径重置。
func TestDayRollArchives(t *testing.T) {
	s := newTestStats(t)
	day1 := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	s.ApplyPoll([]Account{acct1(1000)}, day1)
	s.AddUsage("workbuddy/m1", 30, 20, 0.5, 400, 2, 0, false, day1)
	if snap := s.Snapshot(day1); snap.Tokens != 50 {
		t.Fatalf("今日 tokens = %d, want 50", snap.Tokens)
	}

	day2 := day1.Add(24 * time.Hour)
	s.ApplyPoll([]Account{acct1(1000)}, day2) // 触发跨天
	snap := s.Snapshot(day2)
	if snap.Today != todayKey(day2) {
		t.Fatalf("today = %s, want %s", snap.Today, todayKey(day2))
	}
	if snap.Tokens != 0 || snap.CreditOutExact != 0 || len(snap.Models) != 0 {
		t.Fatalf("跨天后今日口径未清零: tokens=%d exact=%.2f models=%d", snap.Tokens, snap.CreditOutExact, len(snap.Models))
	}
	if s.tokenDays[todayKey(day1)] != 50 || s.reqDays[todayKey(day1)] != 1 {
		t.Fatalf("归档未落历史 map: tokens=%d reqs=%d", s.tokenDays[todayKey(day1)], s.reqDays[todayKey(day1)])
	}
	if len(s.modelDays[todayKey(day1)]) != 1 {
		t.Fatalf("模型消耗未归档: %+v", s.modelDays)
	}
	if snap.TokenUsage.Days7 != 50 {
		t.Fatalf("7 日窗口应含归档 tokens: %d", snap.TokenUsage.Days7)
	}
	// 零值不入档：无消耗的日子不产生空历史键（Days = 有消耗的天数）
	if len(s.tokenDays) != 1 {
		t.Fatalf("不应产生空历史键: %+v", s.tokenDays)
	}
	// 跨天滚动即时落盘（不依赖 RunFlusher 的 5s 周期）：昨日归档在滚动当刻已持久化
	if _, err := os.Stat(s.file); err != nil {
		t.Fatalf("跨天滚动应即时落盘: %v", err)
	}
}

func TestMilestoneToasts(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{acct1(500)}, tNow(1, 9))
	s.ApplyPoll([]Account{acct1(380)}, tNow(1, 10)) // 消耗 120 → 第 1 个里程碑
	snap := s.Snapshot(tNow(1, 10))
	if snap.MilestoneCount != 1 || len(snap.ToastQueue) != 1 {
		t.Fatalf("milestone=%d toasts=%d, want 1/1", snap.MilestoneCount, len(snap.ToastQueue))
	}
	// 再耗 350 → 共 470 → 跨 3 个台阶，合并为一条（文案含 +3）
	s.ApplyPoll([]Account{acct1(30)}, tNow(1, 11))
	snap = s.Snapshot(tNow(1, 11))
	if snap.MilestoneCount != 4 || snap.MilestoneTotal != 470 {
		t.Fatalf("milestone=%d total=%d, want 4/470", snap.MilestoneCount, snap.MilestoneTotal)
	}
	if len(snap.ToastQueue) != 2 {
		t.Fatalf("单轮跨多台阶应合并为一条（共 2 条）: got %d", len(snap.ToastQueue))
	}
	last := snap.ToastQueue[len(snap.ToastQueue)-1]
	if !strings.Contains(last.Body, "+3 个") {
		t.Fatalf("跨台阶文案应含 +3: %s", last.Body)
	}
	if !strings.HasPrefix(last.ID, "2026-10-01-m") {
		t.Fatalf("Toast ID 应带日键: %s", last.ID)
	}
}

func TestPlatformsSortedByCredits(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{
		acct1(100),
		{UID: "t1", Group: "traework", Credits: 9999},
	}, tNow(1, 9))
	snap := s.Snapshot(tNow(1, 9))
	if len(snap.Platforms) != 2 || snap.Platforms[0].Group != "traework" {
		t.Fatalf("平台行应按余额降序: %+v", snap.Platforms)
	}
	// 合计与平台行同源
	var sum int64
	for _, r := range snap.Platforms {
		sum += r.Credits
	}
	if sum != snap.TotalCredits {
		t.Fatalf("合计与平台行不同源: %d vs %d", sum, snap.TotalCredits)
	}
}

func TestZeroValueSnapshot(t *testing.T) {
	s := newTestStats(t) // 未喂过任何数据
	snap := s.Snapshot(tNow(1, 9))
	if snap.TotalAccounts != 0 || len(snap.Platforms) != 0 || snap.CreditOut != 0 {
		t.Fatalf("零值快照应为空: %+v", snap)
	}
	if snap.Version != Version {
		t.Fatalf("快照应带引擎语义版本: %s", snap.Version)
	}
}

// 异常账号区块：收录停用/整号冷却/错误累计三类，排序=严重度→解冻时刻；
// 健康账号不入列，无异常时下发空切片（前端隐藏区块）。
func TestAbnormalRows(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "ok", Group: "workbuddy", Nickname: "正常号", Credits: 100},
		{UID: "dis", Group: "workbuddy", Nickname: "停用号", Credits: 10, Disabled: true, Reason: "session dead"},
		{UID: "cool", Group: "traework", Nickname: "冷却号", Credits: 20, Cooling: true, Reason: "429 rate limit", Until: "10-08 02:00"},
		{UID: "er", Group: "qodercn", Nickname: "预警号", Credits: 40, ErrCount: 2},
	}, now)

	rows := s.Snapshot(now).Abnormal
	if len(rows) != 3 {
		t.Fatalf("应收录 3 行（健康号不入列）: %+v", rows)
	}
	wantOrder := []string{"dis", "cool", "er"}
	for i, uid := range wantOrder {
		if rows[i].UID != uid {
			t.Fatalf("第 %d 行应为 %s: %+v", i, uid, rows)
		}
	}
	if rows[0].Level != "disabled" || rows[0].Reason != "session dead" {
		t.Fatalf("停用行错误: %+v", rows[0])
	}
	if rows[1].Level != "cool" || rows[1].Until != "10-08 02:00" || rows[1].Reason != "429 rate limit" {
		t.Fatalf("整号冷却行错误: %+v", rows[1])
	}
	if rows[2].Level != "err" || rows[2].ErrCount != 2 {
		t.Fatalf("错误累计行错误: %+v", rows[2])
	}

	// 全部恢复正常 → 空切片（前端隐藏）
	s.ApplyPoll([]Account{
		{UID: "ok", Group: "workbuddy", Nickname: "正常号", Credits: 100},
		{UID: "dis", Group: "workbuddy", Nickname: "停用号", Credits: 10},
	}, now.Add(time.Minute))
	if rows := s.Snapshot(now.Add(time.Minute)).Abnormal; len(rows) != 0 {
		t.Fatalf("无异常应下发空: %+v", rows)
	}
}

// 轮换视图：两级排序（临期降序→余额降序）、排除冷却/停用、剔除当前粘性账号；
// 无粘性记录（今日未使用）的平台整行不显示。
func TestRotationView(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "a1", Group: "workbuddy", Nickname: "A1", Credits: 100, Expiring: 0},
		{UID: "a2", Group: "workbuddy", Nickname: "A2", Credits: 500, Expiring: 0},
		{UID: "a3", Group: "workbuddy", Nickname: "A3", Credits: 300, Expiring: 200}, // 临期最高 → 第 1
		{UID: "a4", Group: "workbuddy", Nickname: "A4", Credits: 900, Cooling: true}, // 冷却排除
		{UID: "a5", Group: "workbuddy", Nickname: "A5", Credits: 900, Disabled: true},
		{UID: "t1", Group: "traework", Nickname: "T1", Credits: 50},
		{UID: "t2", Group: "traework", Nickname: "T2", Credits: 30}, // 第二账号：单账号组不进轮换视图
	}, now)
	s.ApplySticky("workbuddy", "a2", 30, 50) // 当前粘性 = a2

	snap := s.Snapshot(now)
	// traework 无粘性记录 → 整平台不显示
	if len(snap.Rotation) != 1 || snap.Rotation[0].Group != "workbuddy" {
		t.Fatalf("应只有 workbuddy 一个平台视图: %+v", snap.Rotation)
	}
	wb := &snap.Rotation[0]
	if wb.Current == nil || wb.Current.UID != "a2" || wb.Current.Sticky != "30/50" {
		t.Fatalf("当前粘性账号错误: %+v", wb)
	}
	// 健康 WB 候选=a1/a2/a3（a4 冷却、a5 停用排除），剔除当前粘性 a2 后剩 2 个：
	// a3（临期 200）排第 1，a1（100 分）第 2——两级排序生效
	if len(wb.Next) != 2 || wb.Next[0].UID != "a3" || wb.Next[1].UID != "a1" {
		t.Fatalf("接棒顺序错误: %+v", wb.Next)
	}

	// traework 后来有使用记录 → 平台出现且 Current 正确
	s.ApplySticky("traework", "t1", 5, 50)
	snap = s.Snapshot(now)
	if len(snap.Rotation) != 2 {
		t.Fatalf("traework 有记录后应有 2 个平台视图: %d", len(snap.Rotation))
	}
}

// 单账号组（组内账号总数 < 2）不出现在轮换视图——无接棒候选（Current 恒为自己、
// Next 恒空），纯噪音。自然覆盖：自定义渠道伪账号（R66：UID "custom-<名>"）、
// oczen 匿名号（唯一且不可增删）；platformRows 等聚合视图不受影响。
func TestRotationSkipsSingleAccountGroups(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "custom-sgproxy", Group: "sgproxy", Nickname: "sgproxy (自定义)", Credits: 500},
		{UID: "custom-cliproxy", Group: "cliproxy", Nickname: "cliproxy (自定义)", Credits: 300},
		{UID: "oczen-anonymous", Group: "oczen", Nickname: "匿名", Credits: 800},
		{UID: "a1", Group: "workbuddy", Nickname: "A1", Credits: 100},
		{UID: "a2", Group: "workbuddy", Nickname: "A2", Credits: 200},
	}, now)
	s.ApplySticky("sgproxy", "custom-sgproxy", 1, 50)
	s.ApplySticky("oczen", "oczen-anonymous", 2, 50)
	s.ApplySticky("workbuddy", "a1", 5, 50)

	snap := s.Snapshot(now)
	for _, v := range snap.Rotation {
		if v.Group == "sgproxy" || v.Group == "cliproxy" || v.Group == "oczen" {
			t.Fatalf("单账号组不应出现在轮换视图: %+v", v)
		}
	}
	// 多账号渠道不受影响
	if len(snap.Rotation) != 1 || snap.Rotation[0].Group != "workbuddy" {
		t.Fatalf("workbuddy 应正常显示: %+v", snap.Rotation)
	}
	// platformRows 不受影响：单账号渠道（自定义/oczen 匿名）照常入平台表
	foundSgproxy, foundOczen := false, false
	for _, r := range snap.Platforms {
		if r.Group == "sgproxy" && r.Accounts == 1 {
			foundSgproxy = true
		}
		if r.Group == "oczen" && r.Accounts == 1 {
			foundOczen = true
		}
	}
	if !foundSgproxy || !foundOczen {
		t.Fatalf("单账号渠道应仍在平台表: %+v", snap.Platforms)
	}
}

// 粘性账号被删后整视图退场（Current 无法定位即 continue）；下一轮上游重建
// 粘性记录后自然恢复。另覆盖：组内只剩单账号时同样不生成视图。
func TestRotationCurrentEvaporates(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{
		{UID: "a1", Group: "workbuddy", Credits: 100},
		{UID: "a2", Group: "workbuddy", Credits: 80},
	}, now)
	s.ApplySticky("workbuddy", "a1", 10, 50)
	if len(s.Snapshot(now).Rotation) != 1 {
		t.Fatalf("前置失败：应有 1 个平台视图")
	}
	// a1 被删（粘性 uid 无法定位）→ 即使组内仍有 a2，整视图退场
	s.ApplyPoll([]Account{{UID: "a2", Group: "workbuddy", Credits: 80}}, now.Add(time.Minute))
	if snap := s.Snapshot(now.Add(time.Minute)); len(snap.Rotation) != 0 {
		t.Fatalf("粘性账号消失后平台视图应退场: %+v", snap.Rotation)
	}
	// 恢复多账号 + 新粘性记录 → 视图回来
	s.ApplyPoll([]Account{
		{UID: "a2", Group: "workbuddy", Credits: 80},
		{UID: "a3", Group: "workbuddy", Credits: 60},
	}, now.Add(2*time.Minute))
	s.ApplySticky("workbuddy", "a2", 1, 50)
	if snap := s.Snapshot(now.Add(2 * time.Minute)); len(snap.Rotation) != 1 || snap.Rotation[0].Current.UID != "a2" {
		t.Fatalf("重建粘性后视图应恢复: %+v", snap.Rotation)
	}
}

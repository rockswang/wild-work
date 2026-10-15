package stats

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeEventLog 落一份事件日志文件并返回路径（行首时间戳自动用今天）。
func writeEventLog(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func todaySlash() string { return time.Now().Format("2006/01/02") }

// 事件口径：统一成功行 + TW claim/status 行，幂等重扫不双计，status 修正数额。
func TestEventIncomeIdempotent(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	// 事件收入按 uid 反查渠道归属平台行，须先喂入账号
	s.ApplyPoll([]Account{
		{UID: "w1", Group: "workbuddy", Nickname: "W1", Credits: 1000},
		{UID: "t1", Group: "traework", Nickname: "T1", Credits: 1000},
		{UID: "t2", Group: "traework", Nickname: "T2", Credits: 1000},
	}, now)
	day := todaySlash()
	path := writeEventLog(t, []string{
		day + " 10:19:30.140862 checkin platform=workbuddy uid=w1 ok=true msg=ok remain=3600 has_remain=true",
		day + " 10:20:30.140862 checkin result platform=traework uid=t1 ok=true msg=ok remain=281 has_remain=true",
		day + " 10:21:30.140862 traework checkin claim response uid=t2 code=0 msg=success",
		day + " 10:21:31.140862 traework checkin status uid=t2 checked_in=true credits=200 enable=true",
		// 同一事件的无平台重复行形态：不应另记
		day + " 10:21:31.140862 checkin uid=t2 ok=true msg=ok remain=281 has_remain=true",
	})
	s.RescanEventLog(path)
	s.RescanEventLog(path) // 幂等：全量重扫不双计
	snap := s.Snapshot(time.Now())
	if want := int64(100 + 100 + 200); snap.CreditIn != want {
		t.Fatalf("credit_in = %d, want %d（WB 100 + TW 100 + TW status 修正 200）", snap.CreditIn, want)
	}
	// 平台行收入归属：workbuddy 100、traework 300（100+200）
	byGroup := map[string]PlatformRow{}
	for _, r := range snap.Platforms {
		byGroup[r.Group] = r
	}
	if wb := byGroup["workbuddy"]; wb.CreditIn != 100 {
		t.Fatalf("workbuddy 行 credit_in = %d, want 100", wb.CreditIn)
	}
	if tw := byGroup["traework"]; tw.CreditIn != 300 {
		t.Fatalf("traework 行 credit_in = %d, want 300", tw.CreditIn)
	}
}

// TW status 行 credits **双向精确修正**：官方调低签到数额时，统一行默认表兜底后
// 必须能被同日 status 行下修（只上调不下修会在官方调低时虚记）；上调方向同样生效。
func TestEventTWStatusCorrectsBothWays(t *testing.T) {
	s := newTestStats(t)
	day := todaySlash()
	path := writeEventLog(t, []string{
		// 统一行无数额 → 默认表 100；status 行 80 → 下修
		day + " 07:15:00.100000 checkin result platform=traework uid=t1 ok=true msg=ok remain=2007 has_remain=true",
		day + " 07:15:00.200000 traework checkin status uid=t1 checked_in=true credits=80 enable=true",
		// 统一行无数额 → 默认表 100；status 行 200 → 上修（官方加码场景）
		day + " 07:15:01.100000 checkin result platform=traework uid=t2 ok=true msg=ok remain=2107 has_remain=true",
		day + " 07:15:01.200000 traework checkin status uid=t2 checked_in=true credits=200 enable=true",
		// 同值 status 重复行（晚间槽复述同日数额）：幂等不双计
		day + " 21:00:00.000000 traework checkin status uid=t1 checked_in=true credits=80 enable=true",
	})
	s.RescanEventLog(path)
	s.RescanEventLog(path) // 幂等：全量重扫不双计、不回摆
	if got, want := s.Snapshot(time.Now()).CreditIn, int64(80+200); got != want {
		t.Fatalf("credit_in = %d, want %d（t1 下修 80 + t2 上修 200）", got, want)
	}
}

// 事件口径跳过规则：已签到/活跃保活无入账、ok=false 不采信、无平台/未知平台不记。
func TestEventSkipRules(t *testing.T) {
	s := newTestStats(t)
	day := todaySlash()
	path := writeEventLog(t, []string{
		day + " 08:09:18.745901 checkin platform=traework uid=t1 ok=true msg=已签到 remain=1572 has_remain=true",
		day + " 08:09:19.745901 checkin platform=workbuddy uid=w1 ok=true msg=活跃保活 remain=3600 has_remain=true",
		day + " 08:09:20.745901 checkin platform=traework uid=t2 ok=false msg=checkin claim code=9074 msg=当前参与用户太多，请稍后再试 remain=1038 has_remain=true",
		day + " 08:09:21.745901 checkin uid=t3 ok=true msg=ok remain=100 has_remain=true", // 无平台
		day + " 08:09:22.745901 checkin platform=future uid=f1 ok=true msg=ok remain=1 has_remain=true",
		day + " 08:09:23.745901 checkin failed platform=traework uid=t4 err=已签到",
		day + " 08:09:24.745901 traework checkin claim response uid=t5 code=9074 msg=当前参与用户太多，请稍后再试",
		day + " 08:09:25.745901 qodercn checkin daily uid=q1 ok=true claimed=false amount=50 msg=已领过",
	})
	s.RescanEventLog(path)
	if got := s.Snapshot(time.Now()).CreditIn; got != 0 {
		t.Fatalf("以上行均不应产生收入: credit_in = %d", got)
	}
}

// 事件口径：非今日日志行过滤（跨天重扫不误记昨日收入）。
func TestEventYesterdayFiltered(t *testing.T) {
	s := newTestStats(t)
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006/01/02")
	path := writeEventLog(t, []string{
		yesterday + " 10:19:30.140862 checkin platform=workbuddy uid=w1 ok=true msg=ok remain=3600 has_remain=true",
	})
	s.RescanEventLog(path)
	if got := s.Snapshot(time.Now()).CreditIn; got != 0 {
		t.Fatalf("昨日行不应记账: credit_in = %d", got)
	}
}

// 事件口径：qoder 系取日志 amount，claimed=true 才计；campaigns 行同款（旧格式兼容）。
func TestQoderEventAmount(t *testing.T) {
	s := newTestStats(t)
	day := todaySlash()
	path := writeEventLog(t, []string{
		day + " 09:00:01.000000 qodercn checkin daily uid=q1 ok=true claimed=true amount=50 msg=ok",
		day + " 09:00:02.000000 qodercom checkin campaigns uid=q2 ok=true claimed=true amount=80 msg=ok",
	})
	s.RescanEventLog(path)
	if got := s.Snapshot(time.Now()).CreditIn; got != 130 {
		t.Fatalf("credit_in = %d, want 130（50+80）", got)
	}
}

// TestRaccoonEventAmount 小浣熊签到收入（R65）：结构化行仅 claimed 计收入，
// 且金额取上游回执的实发数额（每日 300，非默认表兜底）。
//
// 为什么必须锁：该行形状与 qoder 系共用解析器（reQoderClaimStatus）。若渠道侧
// 改了日志字段顺序/命名而这里没同步，签到收入会**静默归零**（面板「今日签到收入」
// 恒 0，但签到状态本身正常显示——比面板失效更隐蔽，同 R34 家族）。
func TestRaccoonEventAmount(t *testing.T) {
	s := newTestStats(t)
	day := todaySlash()
	path := writeEventLog(t, []string{
		// 领到 300：入账
		day + " 10:27:04.000000 raccoon checkin setting_info uid=r1 status=claimed claimed=true amount=300 msg=签到成功 +300（每日积分发放） err=<nil> remain=3896",
		// 今日已领：无新入账
		day + " 10:27:05.000000 raccoon checkin setting_info uid=r2 status=already_claimed claimed=false amount=0 msg=今日已领取 err=<nil> remain=3873",
		// 会话失效：无入账
		day + " 10:27:06.000000 raccoon checkin setting_info uid=r3 status=no_token claimed=false amount=0 msg=登录态失效，待刷新后重试 err=401 remain=0",
	})
	s.RescanEventLog(path)
	s.RescanEventLog(path) // 幂等：重扫不双计
	if got := s.Snapshot(time.Now()).CreditIn; got != 300 {
		t.Fatalf("credit_in = %d, want 300（仅 claimed 计入）", got)
	}
}

// TestRaccoonEventShapeNotDegraded 守门：小浣熊签到行必须被**严格正则**识别，
// 不得落进「形状像但解析不了」的降级态（那意味着收入统计已失准）。
func TestRaccoonEventShapeNotDegraded(t *testing.T) {
	s := newTestStats(t)
	day := todaySlash()
	path := writeEventLog(t, []string{
		day + " 10:27:04.000000 raccoon checkin setting_info uid=r1 status=claimed claimed=true amount=300 msg=签到成功 +300 err=<nil> remain=3896",
		day + " 10:27:05.000000 raccoon checkin setting_info uid=r2 status=already_claimed claimed=false amount=0 msg=今日已领取 err=<nil> remain=3873",
	})
	s.RescanEventLog(path)
	if s.evUnparsed != 0 {
		t.Fatalf("小浣熊签到行未被严格正则识别：unparsed=%d（收入统计会静默失准）", s.evUnparsed)
	}
	if s.evDegraded {
		t.Fatal("不应进入解析降级态")
	}
}

// 结构化新格式：统一行可选 retryable 字段 + qoder 结构化 status 行
// （仅 claimed 计收入）。旧格式行为由本文件其余事件测试守住（双向兼容）。
func TestEventFormatsStructured(t *testing.T) {
	s := newTestStats(t)
	day := todaySlash()
	path := writeEventLog(t, []string{
		// scheduler.go:429（带 retryable）
		day + " 10:19:30.140862 checkin result platform=workbuddy uid=w1 ok=true retryable=false msg=ok remain=3600 has_remain=true",
		// app.go:2074（无 retryable 的旧版式，继续有效）
		day + " 10:19:31.140862 checkin platform=traework uid=t1 ok=true msg=ok remain=281 has_remain=true",
		// qoder 结构化行：claimed 入账 100；already_claimed / no_campaign 不计
		day + " 10:19:32.140862 qodercn checkin campaigns uid=q1 status=claimed claimed=true amount=100 msg=已领取 err=<nil>",
		day + " 10:19:33.140862 qodercom checkin campaigns uid=q2 status=already_claimed claimed=false amount=0 msg=今日已领 err=<nil>",
		day + " 10:19:34.140862 qodercom checkin campaigns uid=q3 status=no_campaign claimed=false amount=0 msg=无活动 err=none",
	})
	s.RescanEventLog(path)
	s.RescanEventLog(path) // 幂等：重扫不双计
	if got, want := s.Snapshot(time.Now()).CreditIn, int64(100+100+100); got != want {
		t.Fatalf("credit_in = %d, want %d（WB 100 + TW 100 + qoder claimed 100）", got, want)
	}
}

// 解析健康度自检：形状像签到结果但严格正则全不命中 → 边沿告警一次；
// 恢复正常 → 恢复告警一次（周期重扫不节流会刷屏）。
func TestEventScanDegradedAlert(t *testing.T) {
	var logged []string
	s, err := New(t.TempDir(), func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	day := todaySlash()
	badPath := writeEventLog(t, []string{
		// 上游改字段名（ok= → okay=）：形状探针命中、严格正则全失配 → 降级
		day + " 10:00:00.000000 checkin result platform=workbuddy uid=w1 okay=true",
	})
	s.RescanEventLog(badPath)
	s.RescanEventLog(badPath)
	if s.evDegraded != true {
		t.Fatalf("应进入解析降级态: strict=%d unparsed=%d", s.evStrict, s.evUnparsed)
	}
	n := 0
	for _, l := range logged {
		if strings.Contains(l, "签到日志解析失效") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("降级告警应边沿触发一次，实际 %d 次: %v", n, logged)
	}
	// 恢复：正常日志 → 恢复告警一次
	goodPath := writeEventLog(t, []string{
		day + " 10:19:30.140862 checkin platform=workbuddy uid=w1 ok=true msg=ok remain=3600 has_remain=true",
	})
	s.RescanEventLog(goodPath)
	if s.evDegraded != false {
		t.Fatalf("应退出降级态")
	}
	n = 0
	for _, l := range logged {
		if strings.Contains(l, "签到日志解析恢复") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("恢复告警应一次，实际 %d 次", n)
	}
}

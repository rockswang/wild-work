package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 旧格式兼容（accts 曾为 uid->burnSince 字符串 map）：解析失败时丢弃 accts 段、
// 保留 token_days/req_days 历史；flush 一次后即转为新格式。
func TestLoadLegacyStatsJSON(t *testing.T) {
	dir := t.TempDir()
	legacy := `{
  "token_days": {"2026-09-20": 12345},
  "req_days": {"2026-09-20": 7},
  "accts": {"370999939580348": "2026-09-21T22:06:12+08:00"}
}`
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New 兼容旧格式失败: %v", err)
	}
	snap := s.Snapshot(time.Now())
	if snap.TokenUsage.Days != 1 || snap.TokenUsage.Days30 < 12345 {
		t.Fatalf("历史应保留: days=%d days30=%d", snap.TokenUsage.Days, snap.TokenUsage.Days30)
	}
	if snap.TokenUsage.ReqAll < 7 {
		t.Fatalf("req 历史应保留: %d", snap.TokenUsage.ReqAll)
	}
	// 首启后 flush 应转为新格式（accts 为对象或空 map）
	s.mu.Lock()
	s.dirty = true
	s.flushLocked()
	s.mu.Unlock()
	raw, _ := os.ReadFile(filepath.Join(dir, "stats.json"))
	if bytes.Contains(raw, []byte(`"2026-09-21T22:06:12`)) {
		t.Fatalf("落盘后不应残留旧字符串格式: %s", raw)
	}
}

// UTF-8 BOM 容忍：遇 BOM 直接 Unmarshal 会报 invalid character 'ï'，
// 且会误走「旧格式迁移」分支把 accts 全丢（实测踩坑）。
func TestLoadBOMTolerance(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().Format("2006-01-02")
	now := time.Now().Format(time.RFC3339)
	body := fmt.Sprintf(`{
  "token_days": {"2026-09-20": 100},
  "req_days": {},
  "accts": {"a1": {"day": %q, "burn_since": %q, "credit_out": 42}}
}`, today, now)
	raw := append([]byte("\xef\xbb\xbf"), []byte(body)...)
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("BOM 文件应可载入: %v", err)
	}
	snap := s.Snapshot(time.Now())
	if snap.TokenUsage.Days30 < 100 {
		t.Fatalf("历史应保留: %d", snap.TokenUsage.Days30)
	}
	if snap.CreditOut != 42 {
		t.Fatalf("今日差值基线应恢复（BOM 不应误走旧格式分支丢弃 accts）: %d", snap.CreditOut)
	}
}

// 旧差值口径 stats.json（accts 带 credit_in）：载入后收入不继承、花费保留
// （credit_in 字段读取时自然忽略）。
func TestMigrationDropsDiffIncome(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Format(time.RFC3339)
	today := time.Now().Format("2006-01-02")
	old := fmt.Sprintf(`{
  "token_days": {}, "req_days": {},
  "accts": {"a1": {"day": %q, "burn_since": %q, "credit_in": 777, "credit_out": 123}}
}`, today, now)
	if err := os.WriteFile(filepath.Join(dir, "stats.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := s.Snapshot(time.Now())
	if snap.CreditIn != 0 {
		t.Fatalf("旧差值收入不应继承: credit_in = %d", snap.CreditIn)
	}
	if snap.CreditOut != 123 {
		t.Fatalf("旧差值花费应保留: credit_out = %d, want 123", snap.CreditOut)
	}
}

// 今日基线持久化往返：重启后差值花费与 burn 观测起点同窗恢复（首轮喂入前
// Snapshot 口径一致）；非今日/缺字段的基线整块失效。
func TestAcctBaselineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 1000}}, now)
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 850}}, now.Add(time.Minute)) // 花费 150
	s.FlushNow()

	s2, err := New(dir, nil)
	if err != nil {
		t.Fatalf("重开引擎: %v", err)
	}
	snap := s2.Snapshot(time.Now())
	if snap.CreditOut != 150 {
		t.Fatalf("重启后 credit_out = %d, want 150", snap.CreditOut)
	}
	if s2.accts["a"].burnSince.IsZero() {
		t.Fatalf("burn 观测起点未恢复（外推会系统性低估）")
	}
	// 重启后首轮喂入重建基线（hasLast=false）：flush 到重启之间的消耗
	//（850→800 的 50 分）不入账——差值基线重立的固有边界，维持现状。
	s2.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 800}}, now.Add(2*time.Minute))
	if snap := s2.Snapshot(time.Now()); snap.CreditOut != 150 {
		t.Fatalf("重启续记错误: credit_out = %d, want 150（历史 150 恢复 + 间隔消耗丢失）", snap.CreditOut)
	}
}

// 模型消耗落盘：今日随存随恢复（含全局精确计数求和重建）；跨天归档 model_days。
func TestModelPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 用真实「今日」驱动（day 守卫以 New 时的本地日为基准，固定日期会被判跨天残留）
	day1 := time.Now()
	s.AddUsage("workbuddy/m1", 30, 20, 0.5, 600, 2, 0, false, day1)
	s.AddUsage("workbuddy/m1", 10, 5, 0.25, 400, 1, 0, false, day1)
	s.AddUsage("traework/m2", 100, 50, 0, 900, 5, 0, false, day1)
	s.FlushNow()

	// 重启：今日模型 + 今日全局计数（tokens/reqs/exact）恢复
	s2, err := New(dir, nil)
	if err != nil {
		t.Fatalf("重开引擎: %v", err)
	}
	snap := s2.Snapshot(day1)
	if snap.Tokens != 215 || snap.Reqs != 3 || snap.CreditOutExact != 0.75 {
		t.Fatalf("重启后今日计数: tokens=%d reqs=%d exact=%.2f", snap.Tokens, snap.Reqs, snap.CreditOutExact)
	}
	if len(snap.Models) != 2 {
		t.Fatalf("重启后模型数 = %d, want 2", len(snap.Models))
	}
	// 跨天：模型清单归档进 modelDays，今日口径清零
	day2 := day1.Add(24 * time.Hour)
	s2.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 100}}, day2)
	if len(s2.modelDays[todayKey(day1)]) != 2 {
		t.Fatalf("跨天未归档模型清单: %d", len(s2.modelDays[todayKey(day1)]))
	}
	if n := len(s2.Snapshot(day2).Models); n != 0 {
		t.Fatalf("跨天后今日模型未清零: %d", n)
	}
	// 归档数据落盘且再次重启可读（后续分析可用）
	s2.FlushNow()
	s3, err := New(dir, nil)
	if err != nil {
		t.Fatalf("三次 New: %v", err)
	}
	if mm := s3.modelDays[todayKey(day1)]; len(mm) != 2 || mm["workbuddy/m1"].reqs != 2 || mm["workbuddy/m1"].tokens != 65 {
		t.Fatalf("归档数据重启后不完整: %+v", mm)
	}
	// 性能采样随消耗一起归档
	if a := s3.modelDays[todayKey(day1)]["workbuddy/m1"]; a.perfCnt != 2 || a.ttfbSumMs != 1000 || a.genTokens != 25 {
		t.Fatalf("归档性能采样不完整: %+v", a)
	}
}

// 事件收入持久化：flush 后重启（重开引擎）不丢，且重扫同日志仍幂等。
func TestEventIncomePersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	day := todaySlash()
	path := writeEventLog(t, []string{
		day + " 10:19:30.140862 checkin platform=workbuddy uid=w1 ok=true msg=ok remain=3600 has_remain=true",
	})
	s.RescanEventLog(path)
	s.FlushNow()

	s2, err := New(dir, nil)
	if err != nil {
		t.Fatalf("重开引擎: %v", err)
	}
	if got := s2.Snapshot(time.Now()).CreditIn; got != 100 {
		t.Fatalf("重启后 credit_in = %d, want 100", got)
	}
	// 重启后同日志重扫仍幂等（events 恢复 + 重扫去重）
	s2.RescanEventLog(path)
	if got := s2.Snapshot(time.Now()).CreditIn; got != 100 {
		t.Fatalf("重启后重扫 credit_in = %d, want 100", got)
	}
}

// RunFlusher：stop 关闭时完成最后一次落盘（退出路径兜底；进程主动退出
// 仍须调 FlushNow——os.Exit 会抢在 goroutine 前终止）。
func TestRunFlusherFlushesOnStop(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 100}}, now)
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 60}}, now.Add(time.Minute))
	if !s.dirty {
		t.Fatalf("前置失败：应有未落盘数据")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { s.RunFlusher(stop); close(done) }()
	close(stop)
	<-done
	if _, err := os.Stat(filepath.Join(dir, "stats.json")); err != nil {
		t.Fatalf("stop 时应完成一次落盘: %v", err)
	}
}

// flushLocked 原子写：落盘后 stats.json 与 .tmp 不同时残留旧内容（tmp 已被 rename 走）。
func TestFlushAtomicRename(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "a", Group: "workbuddy", Credits: 100}}, now)
	s.FlushNow()
	if _, err := os.Stat(filepath.Join(dir, "stats.json.tmp")); err == nil {
		t.Fatalf("rename 后不应残留 tmp 文件")
	}
	// 内容可被 JSON 反序列化（结构自洽）
	raw, _ := os.ReadFile(filepath.Join(dir, "stats.json"))
	var p persistJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("落盘内容应可解析: %v\n%s", err, raw)
	}
	if p.Accts["a"].CreditOut != 0 {
		t.Fatalf("差值 0 也应写入基线（burnSince 已确立）: %+v", p.Accts)
	}
}

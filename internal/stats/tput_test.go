package stats

import (
	"testing"
	"time"
)

// seedModel 在消耗表里登记一个模型行（真实路径里 handler 总是先 AddUsage 再
// AddTput，面板模型表以消耗表为骨架；只有速率没有消耗的模型不应凭空出现在表里）。
func seedModel(t *testing.T, s *Stats, model string, now time.Time) {
	t.Helper()
	s.AddUsage(model, 1, 0, 0, 0, 0, false, now)
}

// 滑动窗口速率 = 最近 60 秒内投递 token 合计 ÷ 60。
// 口径动机：上游每秒吞吐随时变化，单请求比值无法表达任何一秒的真实速率
// （见 docs/adr/accepted/r132-*.md）。
func TestTputWindowRate(t *testing.T) {
	s := newTestStats(t)
	now := time.Unix(1_800_000_000, 0)
	sec := now.Unix()
	seedModel(t, s, "raccoon/m", now)

	// 同一秒内投递 300 token（帧数占比 1/1）→ 窗口 300/60 = 5 tok/s
	s.AddTput("raccoon/m", 300, map[int64]int{sec: 10}, now)
	if got := modelTokPerSec(t, s, now, "raccoon/m"); got != 5 {
		t.Fatalf("窗口速率 = %v, want 5（300 token / 60 秒窗口）", got)
	}

	// 再投 120 token 到 30 秒前那一桶 → (300+120)/60 = 7 tok/s
	s.AddTput("raccoon/m", 120, map[int64]int{sec - 30: 3}, now)
	if got := modelTokPerSec(t, s, now, "raccoon/m"); got != 7 {
		t.Fatalf("窗口速率 = %v, want 7（含 30 秒前的桶）", got)
	}
}

// 窗口外的桶不参与计算，且会被剪掉（防 map 无限增长）。
func TestTputWindowDropsStale(t *testing.T) {
	s := newTestStats(t)
	now := time.Unix(1_800_000_000, 0)
	sec := now.Unix()
	seedModel(t, s, "m", now)

	s.AddTput("m", 600, map[int64]int{sec: 5}, now)
	if got := modelTokPerSec(t, s, now, "m"); got != 10 {
		t.Fatalf("窗口内 = %v, want 10", got)
	}
	// 时间前进 61 秒：老桶全部出窗 → 0（前端据此显示「—」）
	later := now.Add(61 * time.Second)
	if got := modelTokPerSec(t, s, later, "m"); got != 0 {
		t.Fatalf("出窗后 = %v, want 0", got)
	}
	// Snapshot 不剪枝（prune 只在 AddTput 里做），故显式再喂一次触发剪枝
	s.AddTput("m", 60, map[int64]int{later.Unix(): 1}, later)
	if n := len(s.tput.buckets["m"]); n != 1 {
		t.Fatalf("陈旧桶未被剪掉，残留 %d 个（应只剩 later 那一桶）", n)
	}
}

// 已出窗的秒直接拒收（长请求收尾时不应把 token 塞进窗口外）。
func TestTputRejectsOutOfWindowSeconds(t *testing.T) {
	s := newTestStats(t)
	now := time.Unix(1_800_000_000, 0)
	sec := now.Unix()
	seedModel(t, s, "m", now)

	s.AddTput("m", 600, map[int64]int{sec - 120: 5}, now)
	if got := modelTokPerSec(t, s, now, "m"); got != 0 {
		t.Fatalf("窗口外秒不应计入: %v", got)
	}
}

// 未来秒（时钟跳变/上游时间戳异常）同样拒收，防污染窗口。
func TestTputRejectsFutureSeconds(t *testing.T) {
	s := newTestStats(t)
	now := time.Unix(1_800_000_000, 0)
	sec := now.Unix()
	seedModel(t, s, "m", now)

	s.AddTput("m", 600, map[int64]int{sec + 5: 3}, now)
	if got := modelTokPerSec(t, s, now, "m"); got != 0 {
		t.Fatalf("未来秒不应计入: %v", got)
	}
}

// token 按各秒帧数占比分摊，且总和精确等于 tokens（余数落末秒，不产生系统性漂移）。
func TestTputDistributesByChunkShare(t *testing.T) {
	s := newTestStats(t)
	now := time.Unix(1_800_000_000, 0)
	sec := now.Unix()
	seedModel(t, s, "m", now)

	// 第 sec-1 秒 3 帧、第 sec 秒 7 帧，共 100 token → 30 / 70
	s.AddTput("m", 100, map[int64]int{sec - 1: 3, sec: 7}, now)
	b := s.tput.buckets["m"]
	if b[sec-1] != 30 || b[sec] != 70 {
		t.Fatalf("分摊 = %d/%d, want 30/70", b[sec-1], b[sec])
	}

	// 除不尽时余数落末秒，总和必须精确
	s2 := newTestStats(t)
	s2.AddTput("m", 10, map[int64]int{sec - 2: 1, sec - 1: 1, sec: 1}, now)
	var sum int64
	for _, v := range s2.tput.buckets["m"] {
		sum += v
	}
	if sum != 10 {
		t.Fatalf("分摊总和 = %d, want 10（余数必须补齐，否则窗口速率系统性偏低）", sum)
	}
}

// 非法入参一律拒记：非正 token、空帧表、全零帧数、空模型名归 unknown。
func TestTputRejectsInvalid(t *testing.T) {
	s := newTestStats(t)
	now := time.Unix(1_800_000_000, 0)
	sec := now.Unix()
	seedModel(t, s, "m", now)

	s.AddTput("m", 0, map[int64]int{sec: 1}, now)
	s.AddTput("m", -5, map[int64]int{sec: 1}, now)
	s.AddTput("m", 100, nil, now)
	s.AddTput("m", 100, map[int64]int{sec: 0}, now)
	if got := modelTokPerSec(t, s, now, "m"); got != 0 {
		t.Fatalf("非法入参不应产生速率: %v", got)
	}
	// 空模型名归 unknown（与 AddUsage 同口径）
	s.AddTput("", 120, map[int64]int{sec: 1}, now)
	if b := s.tput.buckets["unknown"]; b[sec] != 120 {
		t.Fatalf("空模型名应归 unknown: %+v", s.tput.buckets)
	}
}

// modelTokPerSec 从快照里取某模型的滑动窗口速率（测试助手）。
func modelTokPerSec(t *testing.T, s *Stats, now time.Time, model string) float64 {
	t.Helper()
	for _, m := range s.Snapshot(now).Models {
		if m.Model == model {
			return m.TokPerSec
		}
	}
	return 0
}

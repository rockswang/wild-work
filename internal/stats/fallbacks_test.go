// 降级链事件记账测试（fallbacks.go）：记事件 / 恢复 / 去重 / 环形上限 / 快照序。
package stats

import (
	"testing"
	"time"
)

// TestFallbackEventLifecycle 完整生命周期：降级 Active → 恢复落 Recovered/DurSec。
func TestFallbackEventLifecycle(t *testing.T) {
	s, err := New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 13, 19, 0, 0, 0, time.Local)

	s.NoteFallback("a/x", "b/y", base)
	snap := s.Snapshot(base)
	if len(snap.Fallbacks) != 1 {
		t.Fatalf("降级后事件数 %d，want 1", len(snap.Fallbacks))
	}
	ev := snap.Fallbacks[0]
	if ev.From != "a/x" || ev.To != "b/y" || !ev.Active || ev.At != "19:00:00" || ev.AtUnix != base.Unix() {
		t.Errorf("事件字段不符: %+v", ev)
	}

	// 同一原模型进行中重复上报 → 去重
	s.NoteFallback("a/x", "b/z", base.Add(time.Minute))
	if n := len(s.Snapshot(base).Fallbacks); n != 1 {
		t.Fatalf("进行中重复上报未去重: %d 条", n)
	}

	// 陌生模型恢复探针 → no-op
	s.NoteFallbackRecovery("a/other", base.Add(2*time.Minute))
	if n := len(s.Snapshot(base).Fallbacks); n != 1 {
		t.Fatalf("陌生模型恢复产生事件: %d 条", n)
	}
	// 恢复前快照仍 Active
	if ev := s.Snapshot(base).Fallbacks[0]; !ev.Active {
		t.Fatal("恢复前事件应为 Active")
	}

	// 恢复 → 落 Recovered + DurSec，Active 翻 false
	s.NoteFallbackRecovery("a/x", base.Add(5*time.Minute))
	snap = s.Snapshot(base.Add(5 * time.Minute))
	ev = snap.Fallbacks[0]
	if ev.Active {
		t.Fatal("恢复后仍标记 Active")
	}
	if ev.Recovered != "19:05:00" || ev.DurSec != 300 {
		t.Errorf("恢复字段不符: recovered=%q durSec=%d，want 19:05:00/300", ev.Recovered, ev.DurSec)
	}
}

// TestFallbackSnapshotNewestFirst 快照最新在前；多次降级各自成行。
func TestFallbackSnapshotNewestFirst(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	base := time.Date(2026, 10, 13, 19, 0, 0, 0, time.Local)
	s.NoteFallback("a/first", "b/one", base)
	s.NoteFallbackRecovery("a/first", base.Add(time.Minute))
	s.NoteFallback("a/second", "b/two", base.Add(2*time.Minute))

	rows := s.Snapshot(base).Fallbacks
	if len(rows) != 2 {
		t.Fatalf("事件数 %d，want 2", len(rows))
	}
	if rows[0].From != "a/second" || !rows[0].Active {
		t.Errorf("首行应为最新进行中事件: %+v", rows[0])
	}
	if rows[1].From != "a/first" || rows[1].Active {
		t.Errorf("次行应为已恢复旧事件: %+v", rows[1])
	}
}

// TestFallbackRingCap 环形上限：超限丢最旧，快照恒 ≤ fbKeepMax。
func TestFallbackRingCap(t *testing.T) {
	s, _ := New(t.TempDir(), nil)
	base := time.Date(2026, 10, 13, 19, 0, 0, 0, time.Local)
	for i := 0; i < fbKeepMax+50; i++ {
		s.NoteFallback(time.Duration(i).String()+"/m", "b/x", base.Add(time.Duration(i)*time.Second))
	}
	if n := len(s.Snapshot(base).Fallbacks); n != fbKeepMax {
		t.Fatalf("环形未收敛: %d 条，want %d", n, fbKeepMax)
	}
}

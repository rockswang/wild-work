package ledger

import (
	"testing"
	"time"
)

// 月末的月份运算必须走 monthTagShift。
//
// 直接 t.AddDate(0,-N,0) 在 29/30/31 日会溢出：10 月 31 日减 1 个月得到「9 月 31 日」，
// Go 规整成 10 月 1 日 —— 上月标签等于本月标签，保留期 cutoff 随之跳过一格。

func TestMonthTagShiftAtMonthEnd(t *testing.T) {
	cases := []struct {
		date string
		back int
		want string
	}{
		{"2026-10-31", 1, "202609"},
		{"2026-03-31", 1, "202602"},
		{"2026-03-29", 1, "202602"},
		{"2026-05-31", 1, "202604"},
		{"2026-12-31", 1, "202611"},
		{"2026-01-31", 1, "202512"},
		{"2026-10-31", 6, "202604"}, // 直接 AddDate 会得到 202605（多删一个月）
		{"2026-04-15", 1, "202603"},
	}
	for _, c := range cases {
		ts, err := time.ParseInLocation("2006-01-02", c.date, time.Local)
		if err != nil {
			t.Fatalf("用例日期非法 %s: %v", c.date, err)
		}
		if got := monthTagShift(ts, c.back); got != c.want {
			t.Errorf("monthTagShift(%s, %d) = %s，期望 %s", c.date, c.back, got, c.want)
		}
	}
}

// TestMonthTagShiftIsStableAcrossMonthEnd 同一「月内任意一天」得到同一个上月标签，
// 这样 31 日与 30 日启动的清理窗口不会因为日期不同而抖动。
func TestMonthTagShiftIsStableAcrossMonthEnd(t *testing.T) {
	loc := time.Local
	want := "202609"
	for day := 27; day <= 31; day++ {
		ts := time.Date(2026, 10, day, 12, 0, 0, 0, loc)
		if got := monthTagShift(ts, 1); got != want {
			t.Errorf("2026-10-%02d 的上月标签 = %s，期望 %s", day, got, want)
		}
	}
}

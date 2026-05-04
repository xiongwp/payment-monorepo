package service

import (
	"testing"
	"time"
)

// TestComputeCutDate_BoundarySemantics 验证业务日切语义：
//   - 当日 boundary 之前 → 归属"昨天"业务日
//   - 当日 boundary 时/之后 → 归属"今天"业务日
//
// 这是 booking 入口处计算 cut_date 的唯一规则，整个日切试算平衡都依赖它正确。
func TestComputeCutDate_BoundarySemantics(t *testing.T) {
	tz, err := time.LoadLocation("Asia/Manila")
	if err != nil {
		t.Skipf("tzdata not available: %v", err)
	}
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		// scheduled_time = 10:00:00
		{"midnight: yesterday", time.Date(2026, 4, 27, 0, 0, 0, 0, tz), "2026-04-26"},
		{"08:00: yesterday", time.Date(2026, 4, 27, 8, 0, 0, 0, tz), "2026-04-26"},
		{"09:59:59: yesterday", time.Date(2026, 4, 27, 9, 59, 59, 0, tz), "2026-04-26"},
		{"10:00:00 exact: today", time.Date(2026, 4, 27, 10, 0, 0, 0, tz), "2026-04-27"},
		{"10:00:01: today", time.Date(2026, 4, 27, 10, 0, 1, 0, tz), "2026-04-27"},
		{"23:59:59: today", time.Date(2026, 4, 27, 23, 59, 59, 0, tz), "2026-04-27"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeCutDate(tt.t, 10, 0, 0, tz)
			if got != tt.want {
				t.Fatalf("ComputeCutDate(%s) = %q, want %q", tt.t.Format(time.RFC3339), got, tt.want)
			}
		})
	}
}

// 跨月/跨年边界
func TestComputeCutDate_CrossMonthYear(t *testing.T) {
	tz := time.UTC
	// 月初早于 cut_time → 归属上月最后一天
	if got := ComputeCutDate(time.Date(2026, 5, 1, 9, 0, 0, 0, tz), 10, 0, 0, tz); got != "2026-04-30" {
		t.Fatalf("month boundary: got %q want 2026-04-30", got)
	}
	// 年初早于 cut_time → 归属去年 12-31
	if got := ComputeCutDate(time.Date(2026, 1, 1, 9, 0, 0, 0, tz), 10, 0, 0, tz); got != "2025-12-31" {
		t.Fatalf("year boundary: got %q want 2025-12-31", got)
	}
}

// midnight cut_time（00:00:00）等价于"按自然日"切窗
func TestComputeCutDate_MidnightCut(t *testing.T) {
	tz := time.UTC
	// 任意时刻都归属当天，因为 boundary == 00:00:00 = 当日开始
	cases := []time.Time{
		time.Date(2026, 4, 27, 0, 0, 0, 0, tz),
		time.Date(2026, 4, 27, 12, 30, 0, 0, tz),
		time.Date(2026, 4, 27, 23, 59, 59, 0, tz),
	}
	for _, ts := range cases {
		if got := ComputeCutDate(ts, 0, 0, 0, tz); got != "2026-04-27" {
			t.Fatalf("midnight cut at %s: got %q want 2026-04-27", ts.Format(time.RFC3339), got)
		}
	}
}

// parseHHMMSS 边界
func TestParseHHMMSS(t *testing.T) {
	tests := []struct {
		in              string
		wantH, wantM, wantS int
		wantOK          bool
	}{
		{"10:00:00", 10, 0, 0, true},
		{"23:59:59", 23, 59, 59, true},
		{"10:30", 10, 30, 0, true}, // 允许省秒
		{"00:00:00", 0, 0, 0, true},
		{"24:00:00", 0, 0, 0, false},   // 越界
		{"-1:00:00", 0, 0, 0, false},   // 负数
		{"10:60:00", 0, 0, 0, false},   // 分钟越界
		{"10:00:60", 0, 0, 0, false},   // 秒越界
		{"abc", 0, 0, 0, false},        // 非数字
		{"", 0, 0, 0, false},           // 空
		{"10", 0, 0, 0, false},         // 缺分钟
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			h, m, s, ok := parseHHMMSS(tt.in)
			if ok != tt.wantOK || h != tt.wantH || m != tt.wantM || s != tt.wantS {
				t.Fatalf("parseHHMMSS(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
					tt.in, h, m, s, ok, tt.wantH, tt.wantM, tt.wantS, tt.wantOK)
			}
		})
	}
}

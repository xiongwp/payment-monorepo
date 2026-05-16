package ingester

import (
	"testing"
	"time"
)

func TestAdaptiveController_InitialBatch(t *testing.T) {
	c := NewAdaptiveController(0) // 0 → 默认 500
	if c.CurrentBatch() != 500 {
		t.Errorf("default initial = %d, want 500", c.CurrentBatch())
	}
	c2 := NewAdaptiveController(800)
	if c2.CurrentBatch() != 800 {
		t.Errorf("explicit = %d, want 800", c2.CurrentBatch())
	}
}

func TestAdaptiveController_AdjustByLag(t *testing.T) {
	c := NewAdaptiveController(500)
	c.minInterval = 0 // 测试用,允许立即调整

	cases := []struct {
		lag  int64
		want int
	}{
		{50, 200},     // 平稳
		{500, 500},    // 默认
		{5000, 2000},  // 中度
		{20000, 5000}, // 重度
	}
	for _, tc := range cases {
		got := c.Adjust(tc.lag)
		if got != tc.want {
			t.Errorf("Adjust(lag=%d) = %d, want %d", tc.lag, got, tc.want)
		}
	}
}

func TestAdaptiveController_RespectsMinInterval(t *testing.T) {
	c := NewAdaptiveController(500)
	c.minInterval = 200 * time.Millisecond
	c.Adjust(20000) // 调到 5000
	got := c.Adjust(50)  // 应该忽略 (距上次 < 200ms)
	if got != 5000 {
		t.Errorf("expected unchanged 5000, got %d", got)
	}
	// 等 250ms 后再调
	time.Sleep(250 * time.Millisecond)
	got = c.Adjust(50)
	if got != 200 {
		t.Errorf("after interval, expected 200, got %d", got)
	}
}

func TestSuggestLagFromBatch(t *testing.T) {
	cases := []struct {
		ratio   float64
		batch   int
		wantGTE int64
	}{
		{1.0, 500, 5000},  // 满载 → 估计 lag 高
		{0.6, 500, 500},   // 中等
		{0.2, 500, 250},   // 略有 lag
		{0.05, 500, 0},    // 平稳
	}
	for _, tc := range cases {
		got := SuggestLagFromBatch(tc.ratio, tc.batch)
		if got < tc.wantGTE {
			t.Errorf("SuggestLagFromBatch(%.2f, %d) = %d, want >= %d",
				tc.ratio, tc.batch, got, tc.wantGTE)
		}
	}
}

func TestAdaptiveController_StatsTracking(t *testing.T) {
	c := NewAdaptiveController(500)
	c.RecordConsumed(100)
	c.RecordConsumed(200)
	c.RecordCommitted(280)

	s := c.Stats()
	if s.TotalConsumed != 300 || s.TotalCommitted != 280 {
		t.Errorf("stats wrong: %+v", s)
	}
}

package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// ─── blacklist 归一化 ─────────────────────────────────────────────────────────

func TestBlacklist_CaseInsensitive(t *testing.T) {
	bl := NewMemBlacklist()
	ctx := context.Background()
	bl.Add(ctx, "user", "user123", "fraud")
	for _, q := range []string{"user123", "User123", "USER123", "  user123  "} {
		if !bl.Contains(ctx, "user", q) {
			t.Errorf("expected %q to match (normalized)", q)
		}
	}
}

func TestBlacklist_DimensionNormalize(t *testing.T) {
	bl := NewMemBlacklist()
	ctx := context.Background()
	bl.Add(ctx, "User", "x@y.com", "spam")
	if !bl.Contains(ctx, "USER", "X@Y.COM") {
		t.Error("dim USER vs User should match after normalize")
	}
}

func TestBlacklist_RemoveAfterNormalize(t *testing.T) {
	bl := NewMemBlacklist()
	ctx := context.Background()
	bl.Add(ctx, "user", "User123 ", "fraud")
	bl.Remove(ctx, "USER", "user123")
	if bl.Contains(ctx, "user", "user123") {
		t.Error("should be removed despite case + whitespace differences")
	}
}

func TestBlacklist_ListReturnsNormalized(t *testing.T) {
	bl := NewMemBlacklist()
	ctx := context.Background()
	bl.Add(ctx, "user", "Alice", "")
	bl.Add(ctx, "user", "BOB", "")
	bl.Add(ctx, "ip", "1.2.3.4", "")
	out := bl.List(ctx, "User")
	if len(out) != 2 {
		t.Fatalf("expected 2 user entries, got %d", len(out))
	}
	for _, e := range out {
		if e.Dimension != "user" {
			t.Errorf("expected normalized dim 'user', got %q", e.Dimension)
		}
		if e.Value != "alice" && e.Value != "bob" {
			t.Errorf("unexpected normalized value %q", e.Value)
		}
	}
}

// ─── velocity 时间分桶 ────────────────────────────────────────────────────────

func TestVelocity_CountsRecentIncrs(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.Incr(ctx, "user:42", 100)
	}
	if got := c.GetVelocity(ctx, "user:42", 1); got != 5 {
		t.Fatalf("velocity 1min: expected 5, got %d", got)
	}
	if got := c.GetVelocity(ctx, "user:42", 60); got != 5 {
		t.Fatalf("velocity 60min: expected 5, got %d", got)
	}
}

func TestVelocity_PerKeyIsolation(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	c.Incr(ctx, "a", 1)
	c.Incr(ctx, "a", 1)
	c.Incr(ctx, "b", 1)
	if got := c.GetVelocity(ctx, "a", 60); got != 2 {
		t.Errorf("a: expected 2, got %d", got)
	}
	if got := c.GetVelocity(ctx, "b", 60); got != 1 {
		t.Errorf("b: expected 1, got %d", got)
	}
}

func TestVelocity_UnknownKeyReturnsZero(t *testing.T) {
	c := NewMemCounter()
	if got := c.GetVelocity(context.Background(), "never-incremented", 60); got != 0 {
		t.Fatalf("expected 0 for unknown key, got %d", got)
	}
}

// ─── 滚动 N 天聚合 ─────────────────────────────────────────────────────────

func TestRollingDays_TodayOnly(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	c.Incr(ctx, "user:42", 1000)
	c.Incr(ctx, "user:42", 2000)
	if got := c.GetRollingDays(ctx, "user:42", 1); got != 3000 {
		t.Fatalf("rolling 1d: expected 3000, got %d", got)
	}
}

func TestRollingDays_ZeroOrNegativeDays(t *testing.T) {
	c := NewMemCounter()
	c.Incr(context.Background(), "user:42", 100)
	for _, d := range []int{0, -1, -7} {
		if got := c.GetRollingDays(context.Background(), "user:42", d); got != 0 {
			t.Errorf("days=%d: expected 0, got %d", d, got)
		}
	}
}

func TestRollingDays_AcrossPriorDays(t *testing.T) {
	// 直接往 daily map 里塞历史 daily key，验证 rolling 真把它们加进来。
	c := NewMemCounter()
	now := time.Now().UTC()
	for i, amt := range []int64{500, 1000, 1500} { // i=0 今天, i=1 昨天, i=2 前天
		k := now.AddDate(0, 0, -i).Format("20060102") + ":" + "user:42"
		var v atomic.Int64
		v.Add(amt)
		c.daily.Store(k, &v)
	}
	if got := c.GetRollingDays(context.Background(), "user:42", 1); got != 500 {
		t.Errorf("1d: expected 500, got %d", got)
	}
	if got := c.GetRollingDays(context.Background(), "user:42", 2); got != 1500 {
		t.Errorf("2d: expected 1500, got %d", got)
	}
	if got := c.GetRollingDays(context.Background(), "user:42", 3); got != 3000 {
		t.Errorf("3d: expected 3000, got %d", got)
	}
	// 4 天往回但只塞了 3 天 → 仍然 3000（缺失天等于 0）
	if got := c.GetRollingDays(context.Background(), "user:42", 4); got != 3000 {
		t.Errorf("4d: expected 3000, got %d", got)
	}
}

func TestRollingDays_PerKeyIsolation(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	c.Incr(ctx, "a", 100)
	c.Incr(ctx, "b", 999)
	if got := c.GetRollingDays(ctx, "a", 7); got != 100 {
		t.Errorf("a: expected 100, got %d", got)
	}
	if got := c.GetRollingDays(ctx, "b", 7); got != 999 {
		t.Errorf("b: expected 999, got %d", got)
	}
}

func TestVelocityAmount_RecentIncrs(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	c.Incr(ctx, "user:42", 100)
	c.Incr(ctx, "user:42", 250)
	c.Incr(ctx, "user:42", 50)
	if got := c.GetVelocityAmount(ctx, "user:42", 60); got != 400 {
		t.Fatalf("expected 400, got %d", got)
	}
}

func TestVelocityAmount_UnknownKeyReturnsZero(t *testing.T) {
	c := NewMemCounter()
	if got := c.GetVelocityAmount(context.Background(), "never", 60); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
}

func TestVelocity_ExpireOldBucketsManual(t *testing.T) {
	// 直接构造一个 velocityWindow，模拟"60min 之前的桶残留"，验证 expire 清零。
	w := newVelocityWindow()
	now := time.Now()
	curMinute := now.Unix() / 60
	// 在 idx=0 写入 70 分钟前的 stale 数据
	w.bucketStart[0] = curMinute - 70
	w.bucketCount[0] = 999
	w.expireOldBuckets(now)
	if w.bucketCount[0] != 0 {
		t.Fatalf("stale bucket not expired, count=%d", w.bucketCount[0])
	}
	if w.bucketStart[0] != 0 {
		t.Fatalf("stale bucket start not reset, got %d", w.bucketStart[0])
	}
}

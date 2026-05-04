package store

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCounter 计数 inner 调用次数，验证 cache hit 实际拦住了。
type fakeCounter struct {
	getDailyN      atomic.Int64
	getMonthlyN    atomic.Int64
	getVelocityN   atomic.Int64
	getVelAmountN  atomic.Int64
	getRollingN    atomic.Int64
	incrN          atomic.Int64
}

func (f *fakeCounter) GetDaily(_ context.Context, _ string) int64 {
	f.getDailyN.Add(1)
	return 100
}
func (f *fakeCounter) GetMonthly(_ context.Context, _ string) int64 {
	f.getMonthlyN.Add(1)
	return 200
}
func (f *fakeCounter) GetVelocity(_ context.Context, _ string, _ int) int {
	f.getVelocityN.Add(1)
	return 5
}
func (f *fakeCounter) GetVelocityAmount(_ context.Context, _ string, _ int) int64 {
	f.getVelAmountN.Add(1)
	return 500
}
func (f *fakeCounter) GetRollingDays(_ context.Context, _ string, _ int) int64 {
	f.getRollingN.Add(1)
	return 1000
}
func (f *fakeCounter) Incr(_ context.Context, _ string, _ int64) {
	f.incrN.Add(1)
}
func (f *fakeCounter) Purge(_ context.Context, _ string) int { return 0 }

func TestCachedCounter_HitWithinTTL(t *testing.T) {
	f := &fakeCounter{}
	c := NewCachedCounter(f, time.Minute)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		c.GetDaily(ctx, "user:42")
	}
	if got := f.getDailyN.Load(); got != 1 {
		t.Errorf("expected 1 underlying call, got %d", got)
	}
}

func TestCachedCounter_DifferentKeysIsolated(t *testing.T) {
	f := &fakeCounter{}
	c := NewCachedCounter(f, time.Minute)
	ctx := context.Background()
	c.GetDaily(ctx, "a")
	c.GetDaily(ctx, "b")
	c.GetDaily(ctx, "a") // should hit cache
	if got := f.getDailyN.Load(); got != 2 {
		t.Errorf("expected 2 underlying calls, got %d", got)
	}
}

func TestCachedCounter_DifferentMethodsIsolated(t *testing.T) {
	f := &fakeCounter{}
	c := NewCachedCounter(f, time.Minute)
	ctx := context.Background()
	c.GetDaily(ctx, "k")
	c.GetMonthly(ctx, "k")
	c.GetVelocity(ctx, "k", 5)
	c.GetVelocityAmount(ctx, "k", 5)
	c.GetRollingDays(ctx, "k", 7)
	if f.getDailyN.Load() != 1 || f.getMonthlyN.Load() != 1 ||
		f.getVelocityN.Load() != 1 || f.getVelAmountN.Load() != 1 || f.getRollingN.Load() != 1 {
		t.Errorf("each method should hit underlying once, got %+v", f)
	}
}

func TestCachedCounter_VelocityArgIsolated(t *testing.T) {
	// 不同 windowMin 是不同 cache entry。
	f := &fakeCounter{}
	c := NewCachedCounter(f, time.Minute)
	ctx := context.Background()
	c.GetVelocity(ctx, "k", 5)
	c.GetVelocity(ctx, "k", 10)
	c.GetVelocity(ctx, "k", 5) // hit
	if got := f.getVelocityN.Load(); got != 2 {
		t.Errorf("expected 2 underlying calls, got %d", got)
	}
}

func TestCachedCounter_TTLExpires(t *testing.T) {
	f := &fakeCounter{}
	c := NewCachedCounter(f, 1*time.Millisecond)
	ctx := context.Background()
	c.GetDaily(ctx, "k")
	time.Sleep(5 * time.Millisecond)
	c.GetDaily(ctx, "k")
	if got := f.getDailyN.Load(); got != 2 {
		t.Errorf("expected 2 underlying calls after TTL, got %d", got)
	}
}

func TestCachedCounter_IncrInvalidatesKey(t *testing.T) {
	f := &fakeCounter{}
	c := NewCachedCounter(f, time.Minute)
	ctx := context.Background()
	c.GetDaily(ctx, "user:42")
	c.GetMonthly(ctx, "user:42")
	c.GetDaily(ctx, "other") // 不该被失效
	c.Incr(ctx, "user:42", 100)
	c.GetDaily(ctx, "user:42")  // miss after invalidate
	c.GetMonthly(ctx, "user:42") // miss after invalidate
	c.GetDaily(ctx, "other")    // hit
	if got := f.getDailyN.Load(); got != 3 {
		t.Errorf("daily: expected 3 (initial 2 + 1 after invalidate), got %d", got)
	}
	if got := f.getMonthlyN.Load(); got != 2 {
		t.Errorf("monthly: expected 2 (initial + after invalidate), got %d", got)
	}
}

func TestCachedCounter_DisabledTTLPassthrough(t *testing.T) {
	f := &fakeCounter{}
	c := NewCachedCounter(f, 0) // disabled
	ctx := context.Background()
	c.GetDaily(ctx, "k")
	c.GetDaily(ctx, "k")
	c.GetDaily(ctx, "k")
	if got := f.getDailyN.Load(); got != 3 {
		t.Errorf("ttl=0 should passthrough; got %d", got)
	}
}

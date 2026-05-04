package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestAtomic_DailyNoOversell 高并发下日累计原子预扣不能超限。
func TestAtomic_DailyNoOversell(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	const limit int64 = 10000
	const delta int64 = 800
	const goroutines = 100

	var wg sync.WaitGroup
	allowed := 0
	var allowedMu sync.Mutex
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, _ := c.IncrIfBelowDaily(ctx, "user:1", delta, limit, time.Hour)
			if ok {
				allowedMu.Lock()
				allowed++
				allowedMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if int64(allowed)*delta > limit {
		t.Fatalf("oversell: allowed %d * %d = %d > limit %d", allowed, delta, int64(allowed)*delta, limit)
	}
	if allowed != 12 {
		t.Errorf("expected 12 allowed, got %d", allowed)
	}
}

// TestAtomic_DailyCancel 取消（DECRBY 回滚）后允许新预扣。
func TestAtomic_DailyCancel(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	_, ok, _ := c.IncrIfBelowDaily(ctx, "k", 9000, 10000, time.Hour)
	if !ok {
		t.Fatal("first reserve should succeed")
	}
	_, ok, _ = c.IncrIfBelowDaily(ctx, "k", 2000, 10000, time.Hour)
	if ok {
		t.Fatal("second reserve should fail")
	}
	c.CancelDaily(ctx, "k", 9000)
	_, ok, _ = c.IncrIfBelowDaily(ctx, "k", 9000, 10000, time.Hour)
	if !ok {
		t.Fatal("reserve after cancel should succeed")
	}
}

// TestAtomic_CancelFloorsAtZero 取消量超过当前累计时不能变负。
func TestAtomic_CancelFloorsAtZero(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	c.Incr(ctx, "k", 100)
	c.CancelDaily(ctx, "k", 999)
	if got := c.GetDaily(ctx, "k"); got != 0 {
		t.Fatalf("expected floor 0, got %d", got)
	}
}

// TestAtomic_VelocityNoOversell 高并发滑窗笔数原子预扣。
func TestAtomic_VelocityNoOversell(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	const max = 5
	var wg sync.WaitGroup
	allowed := 0
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, _ := c.IncrIfBelowVelocity(ctx, "k", 1, max)
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != max {
		t.Fatalf("expected %d allowed, got %d", max, allowed)
	}
}

// TestAtomic_VelocityAmountNoOversell 滑窗金额原子预扣。
func TestAtomic_VelocityAmountNoOversell(t *testing.T) {
	c := NewMemCounter()
	ctx := context.Background()
	const maxAmt int64 = 1000
	var wg sync.WaitGroup
	allowed := int64(0)
	var mu sync.Mutex
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, _ := c.IncrIfBelowVelocityAmount(ctx, "k", 100, maxAmt, 1)
			if ok {
				mu.Lock()
				allowed += 100
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed > maxAmt {
		t.Fatalf("oversell: allowed=%d > maxAmt=%d", allowed, maxAmt)
	}
}

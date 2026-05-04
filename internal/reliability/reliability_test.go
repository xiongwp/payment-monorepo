package reliability

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := NewBreaker(Config{Name: "x", FailThreshold: 3, OpenDuration: time.Hour})
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("attempt %d should be allowed before threshold", i)
		}
		b.OnFailure()
	}
	if b.State() != Open {
		t.Fatalf("expected OPEN after 3 failures, got %s", b.State())
	}
	if b.Allow() {
		t.Fatal("OPEN breaker should short-circuit")
	}
}

func TestBreaker_HalfOpenProbeSuccessClosesIt(t *testing.T) {
	b := NewBreaker(Config{Name: "x", FailThreshold: 1, OpenDuration: 10 * time.Millisecond, HalfOpenMax: 1})
	b.OnFailure()
	if b.State() != Open {
		t.Fatal("should be open after first fail")
	}
	time.Sleep(20 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("after open_duration the breaker should allow probe")
	}
	if b.State() != HalfOpen {
		t.Fatalf("expected HALF_OPEN, got %s", b.State())
	}
	b.OnSuccess()
	if b.State() != Closed {
		t.Fatalf("expected CLOSED after probe success, got %s", b.State())
	}
}

func TestBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	b := NewBreaker(Config{Name: "x", FailThreshold: 1, OpenDuration: 10 * time.Millisecond, HalfOpenMax: 1})
	b.OnFailure()
	time.Sleep(20 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("expected probe allowed")
	}
	b.OnFailure()
	if b.State() != Open {
		t.Fatalf("probe failure should re-open, got %s", b.State())
	}
}

func TestErrCircuitOpen_IsSentinel(t *testing.T) {
	if !errors.Is(ErrCircuitOpen, ErrCircuitOpen) {
		t.Fatal("ErrCircuitOpen sentinel must be identifiable")
	}
}

func TestMerchantLimiter_PerMerchantIsolation(t *testing.T) {
	l := NewMerchantLimiter(LimitConfig{RPS: 1, Burst: 2}, nil)
	// m1 burst 2 → 2 个连发应过；第 3 个秒级再发应被拒
	if !l.Allow("m1") || !l.Allow("m1") {
		t.Fatal("burst 2 should allow first 2")
	}
	if l.Allow("m1") {
		t.Fatal("3rd should be limited (within burst window)")
	}
	// m2 自己的 bucket 不受 m1 影响
	if !l.Allow("m2") {
		t.Fatal("m2 should be independent of m1")
	}
}

func TestMerchantLimiter_Refill(t *testing.T) {
	l := NewMerchantLimiter(LimitConfig{RPS: 10, Burst: 1}, nil)
	if !l.Allow("m1") {
		t.Fatal("first should pass")
	}
	if l.Allow("m1") {
		t.Fatal("second should be rate-limited (within 100ms)")
	}
	time.Sleep(150 * time.Millisecond)
	if !l.Allow("m1") {
		t.Fatal("after 150ms (>100ms refill), should pass")
	}
}

func TestMerchantLimiter_Disabled(t *testing.T) {
	l := NewMerchantLimiter(LimitConfig{}, nil) // 全零 → disabled
	for i := 0; i < 1000; i++ {
		if !l.Allow("m1") {
			t.Fatal("disabled limiter must always allow")
		}
	}
}

func TestMerchantLimiter_CustomOverride(t *testing.T) {
	l := NewMerchantLimiter(LimitConfig{RPS: 1, Burst: 1},
		map[string]LimitConfig{"vip": {RPS: 100, Burst: 100}})
	// vip：100 burst 应连发都过
	for i := 0; i < 50; i++ {
		if !l.Allow("vip") {
			t.Fatalf("vip should burst freely; failed at %d", i)
		}
	}
	// 普通商户：1 burst 第 2 个就限
	if !l.Allow("normal") || l.Allow("normal") {
		t.Fatal("default tier should limit at 2nd request")
	}
}

func TestMerchantLimiter_Concurrency(t *testing.T) {
	l := NewMerchantLimiter(LimitConfig{RPS: 1000, Burst: 10}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Allow("m1")
		}()
	}
	wg.Wait()
	// 单纯不 panic / data race 即可
}

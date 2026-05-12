package breaker

import (
	"testing"
	"time"
)

func TestClosed_AllowsAll(t *testing.T) {
	b := newBreaker("test|/charge", DefaultConfig())
	if err := b.Allow(); err != nil {
		t.Error("closed should allow")
	}
	for i := 0; i < 10; i++ {
		b.Record(true)
	}
	if b.State() != StateClosed {
		t.Errorf("after 10 successes state = %s, want closed", b.State())
	}
}

func TestOpens_WhenFailureRateExceeds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinRequests = 10
	cfg.WindowSize = 20
	cfg.FailureRate = 0.5
	b := newBreaker("test|/refund", cfg)

	// 10 个失败 + 5 个成功 = 失败率 66.7%
	for i := 0; i < 10; i++ {
		b.Record(false)
	}
	for i := 0; i < 5; i++ {
		b.Record(true)
	}
	if b.State() != StateOpen {
		t.Errorf("state = %s, want open", b.State())
	}
	if err := b.Allow(); err != ErrCircuitOpen {
		t.Errorf("Allow open = %v, want ErrCircuitOpen", err)
	}
}

func TestOpen_TransitionsToHalfOpenAfterDuration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinRequests = 5
	cfg.OpenDuration = 50 * time.Millisecond
	b := newBreaker("test|/refund", cfg)
	for i := 0; i < 10; i++ {
		b.Record(false)
	}
	if b.State() != StateOpen {
		t.Fatal("not open")
	}
	time.Sleep(80 * time.Millisecond)
	if b.State() != StateHalfOpen {
		t.Errorf("after OpenDuration state = %s, want half_open", b.State())
	}
}

func TestHalfOpen_ClosesAfterEnoughSuccesses(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinRequests = 5
	cfg.OpenDuration = 10 * time.Millisecond
	cfg.HalfOpenMinSucc = 3
	b := newBreaker("test|/x", cfg)
	for i := 0; i < 10; i++ {
		b.Record(false)
	}
	time.Sleep(20 * time.Millisecond)
	_ = b.State() // 触发 → half_open

	for i := 0; i < 3; i++ {
		if err := b.Allow(); err != nil {
			t.Fatal("half_open should allow probe")
		}
		b.Record(true)
	}
	if b.State() != StateClosed {
		t.Errorf("after 3 successes state = %s, want closed", b.State())
	}
}

func TestHalfOpen_ReopensOnAnyFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinRequests = 5
	cfg.OpenDuration = 10 * time.Millisecond
	b := newBreaker("test|/x", cfg)
	for i := 0; i < 10; i++ {
		b.Record(false)
	}
	time.Sleep(20 * time.Millisecond)
	_ = b.State()

	if err := b.Allow(); err != nil {
		t.Fatal("half_open should allow")
	}
	b.Record(false)
	if b.State() != StateOpen {
		t.Errorf("after 1 fail in half_open state = %s, want open", b.State())
	}
}

func TestManager_SeparatesPerPath(t *testing.T) {
	mgr := NewManager(DefaultConfig())
	b1 := mgr.Get("stripe", "/v1/refunds")
	b2 := mgr.Get("stripe", "/v1/charges")
	b3 := mgr.Get("stripe", "/v1/refunds") // same key, should return same instance

	if b1 == b2 {
		t.Error("different paths should have different breakers")
	}
	if b1 != b3 {
		t.Error("same key should return same breaker")
	}

	// Path-level 隔离: 一个 path 熔断不影响另一个
	cfg := DefaultConfig()
	cfg.MinRequests = 5
	mgr2 := NewManager(cfg)
	refund := mgr2.Get("stripe", "/refunds")
	charge := mgr2.Get("stripe", "/charges")

	// /refund 失败大量, /charge 全成功
	for i := 0; i < 10; i++ {
		refund.Record(false)
		charge.Record(true)
	}

	if refund.State() != StateOpen {
		t.Error("/refund should be open")
	}
	if charge.State() != StateClosed {
		t.Errorf("/charge state = %s, want closed (path-level isolation)", charge.State())
	}
}

func TestSnapshot(t *testing.T) {
	mgr := NewManager(DefaultConfig())
	mgr.Get("stripe", "/v1/charges")
	mgr.Get("adyen", "/payments")
	snap := mgr.Snapshot()
	if len(snap) != 2 {
		t.Errorf("snapshot has %d entries, want 2", len(snap))
	}
}

package circuitbreaker

import (
	"testing"
	"time"
)

func TestBreaker_ClosedAllowsAll(t *testing.T) {
	b := newBreaker("test", Config{FailThreshold: 3, OpenDuration: 100 * time.Millisecond, HalfOpenMax: 1})
	for i := 0; i < 100; i++ {
		if !b.Allow() {
			t.Fatal("closed breaker should allow all")
		}
		b.RecordSuccess()
	}
	if b.State() != Closed {
		t.Fatalf("state=%s want Closed", b.State())
	}
}

func TestBreaker_OpensAfterThreshold(t *testing.T) {
	b := newBreaker("test", Config{FailThreshold: 3, OpenDuration: 50 * time.Millisecond, HalfOpenMax: 1})
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Closed {
		t.Fatal("should still be closed after 2 failures")
	}
	b.RecordFailure() // 3rd → Open
	if b.State() != Open {
		t.Fatalf("state=%s want Open after 3 failures", b.State())
	}
	if b.Allow() {
		t.Fatal("open breaker should deny")
	}
}

func TestBreaker_TransitionsToHalfOpen(t *testing.T) {
	b := newBreaker("test", Config{FailThreshold: 2, OpenDuration: 30 * time.Millisecond, HalfOpenMax: 1})
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Open {
		t.Fatal("should be open")
	}
	time.Sleep(40 * time.Millisecond)
	// After OpenDuration → HalfOpen
	if !b.Allow() {
		t.Fatal("half-open should allow one probe")
	}
	if b.Allow() {
		t.Fatal("half-open should deny second request (max=1)")
	}
}

func TestBreaker_HalfOpenSuccessCloses(t *testing.T) {
	b := newBreaker("test", Config{FailThreshold: 2, OpenDuration: 20 * time.Millisecond, HalfOpenMax: 1})
	b.RecordFailure()
	b.RecordFailure()
	time.Sleep(25 * time.Millisecond)
	b.Allow() // enters HalfOpen
	b.RecordSuccess()
	if b.State() != Closed {
		t.Fatalf("success in half-open should close, got %s", b.State())
	}
	if !b.Allow() {
		t.Fatal("closed should allow")
	}
}

func TestBreaker_HalfOpenFailureReopens(t *testing.T) {
	b := newBreaker("test", Config{FailThreshold: 2, OpenDuration: 20 * time.Millisecond, HalfOpenMax: 1})
	b.RecordFailure()
	b.RecordFailure()
	time.Sleep(25 * time.Millisecond)
	b.Allow()
	b.RecordFailure()
	if b.State() != Open {
		t.Fatalf("failure in half-open should reopen, got %s", b.State())
	}
}

func TestBreaker_SuccessResetsFailureCount(t *testing.T) {
	b := newBreaker("test", Config{FailThreshold: 3, OpenDuration: time.Second})
	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess() // reset
	b.RecordFailure()
	b.RecordFailure()
	if b.State() != Closed {
		t.Fatal("success should reset failure counter; 2 new failures < threshold 3")
	}
}

func TestRegistry_IsolatesAdapters(t *testing.T) {
	r := NewRegistry(Config{FailThreshold: 2, OpenDuration: time.Second})
	gcash := r.Get("gcash")
	maya := r.Get("maya")
	gcash.RecordFailure()
	gcash.RecordFailure()
	if gcash.State() != Open {
		t.Fatal("gcash should be open")
	}
	if maya.State() != Closed {
		t.Fatal("maya should still be closed (isolated)")
	}
	states := r.States()
	if states["gcash"] != "OPEN" || states["maya"] != "CLOSED" {
		t.Fatalf("states: %v", states)
	}
}

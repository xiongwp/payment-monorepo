package ratelimit

import (
	"testing"
	"time"
)

func TestAllowBurst(t *testing.T) {
	l := New(10, 5) // 10 rps, burst 5
	// 前 5 个立刻过
	for i := 0; i < 5; i++ {
		if !l.Allow("k1") {
			t.Fatalf("denied at i=%d, expected burst=5 to pass", i)
		}
	}
	// 第 6 个被拒
	if l.Allow("k1") {
		t.Fatal("expected deny after burst")
	}
}

func TestAllowDifferentKeys(t *testing.T) {
	l := New(1, 1)
	if !l.Allow("a") {
		t.Fatal("a should pass")
	}
	if l.Allow("a") {
		t.Fatal("a should deny on 2nd call")
	}
	// b 独立 bucket
	if !l.Allow("b") {
		t.Fatal("b should pass independently")
	}
}

func TestRefill(t *testing.T) {
	l := New(100, 1) // 100 rps so 10ms 补 1 token
	l.Allow("x")
	time.Sleep(20 * time.Millisecond)
	if !l.Allow("x") {
		t.Fatal("should pass after refill")
	}
}

func TestGC(t *testing.T) {
	l := New(10, 5)
	l.ttl = 1 * time.Millisecond
	l.Allow("a")
	l.Allow("b")
	time.Sleep(2 * time.Millisecond)
	l.gc()
	if l.Size() != 0 {
		t.Errorf("expected gc to clear all, got %d", l.Size())
	}
}

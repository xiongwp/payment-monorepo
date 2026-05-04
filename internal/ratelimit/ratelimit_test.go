package ratelimit

import (
	"testing"

	"golang.org/x/time/rate"
)

func TestKeyed_ZeroRPSAlwaysAllow(t *testing.T) {
	k := NewKeyed(0, 0)
	for i := 0; i < 100; i++ {
		if !k.Allow("any") {
			t.Fatal("rps=0 should always allow")
		}
	}
}

func TestKeyed_PerKeyIsolation(t *testing.T) {
	// burst=1 → 第一次必通过，第二次同 key 必拒；不同 key 互不影响
	k := NewKeyed(rate.Limit(1), 1)
	if !k.Allow("a") {
		t.Fatal("first allow on a should pass")
	}
	if k.Allow("a") {
		t.Fatal("second allow on a should be rejected (burst=1)")
	}
	if !k.Allow("b") {
		t.Fatal("first allow on b should pass independently of a")
	}
}

func TestKeyed_Len(t *testing.T) {
	k := NewKeyed(rate.Limit(100), 100)
	for _, key := range []string{"x", "y", "z"} {
		k.Allow(key)
	}
	if k.Len() != 3 {
		t.Fatalf("expected 3 keys, got %d", k.Len())
	}
}

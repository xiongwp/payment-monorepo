package ratelimit

import (
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// 已有 ratelimit_test.go;这里补 14 个边界 / 并发 / 热更新场景.

func TestKeyed_ZeroRPSAllowsAll(t *testing.T) {
	k := NewKeyed(0, 0)
	for i := 0; i < 1000; i++ {
		if !k.Allow("ip1") {
			t.Fatalf("rps=0 should allow all, denied at %d", i)
		}
	}
}

func TestKeyed_BurstLimitsInitialSurge(t *testing.T) {
	k := NewKeyed(rate.Limit(1), 3)
	pass := 0
	for i := 0; i < 10; i++ {
		if k.Allow("ip1") {
			pass++
		}
	}
	// burst=3 即首批 3 个能过,剩下需等
	if pass != 3 {
		t.Errorf("burst=3 expected 3 pass, got %d", pass)
	}
}

func TestKeyed_PerKeyIsolation(t *testing.T) {
	k := NewKeyed(rate.Limit(1), 2)
	for i := 0; i < 5; i++ {
		_ = k.Allow("ip-A") // 用完
	}
	// ip-B 仍有自己的桶
	if !k.Allow("ip-B") {
		t.Errorf("ip-B should not be affected by ip-A exhaustion")
	}
}

func TestKeyed_NegativeBurstClampsToOne(t *testing.T) {
	k := NewKeyed(rate.Limit(10), -5)
	if k.burst != 10 { // negative → take rps
		t.Errorf("expect burst=10, got %d", k.burst)
	}
	k2 := NewKeyed(0, -5)
	if k2.burst != 1 {
		t.Errorf("rps=0 burst=-5 expect 1, got %d", k2.burst)
	}
}

func TestKeyed_LenTracks(t *testing.T) {
	k := NewKeyed(rate.Limit(10), 10)
	_ = k.Allow("a")
	_ = k.Allow("b")
	_ = k.Allow("c")
	if l := k.Len(); l != 3 {
		t.Errorf("Len=%d, expect 3", l)
	}
}

func TestKeyed_ConcurrentSameKey(t *testing.T) {
	k := NewKeyed(rate.Limit(100), 10)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = k.Allow("same-key")
		}()
	}
	wg.Wait()
	if l := k.Len(); l != 1 {
		t.Errorf("Len should be 1 for single key, got %d", l)
	}
}

func TestKeyed_MaxKeysEvicts(t *testing.T) {
	k := NewKeyed(rate.Limit(1), 1)
	k.maxKey = 5
	for i := 0; i < 20; i++ {
		_ = k.Allow(string(rune('A' + i)))
	}
	if l := k.Len(); l > 5 {
		t.Errorf("Len=%d should be capped at maxKey=5", l)
	}
}

func TestKeyed_SetLimitUpdatesExisting(t *testing.T) {
	k := NewKeyed(rate.Limit(1), 1)
	_ = k.Allow("x")
	k.SetLimit(rate.Limit(100), 100)
	pass := 0
	for i := 0; i < 50; i++ {
		if k.Allow("x") {
			pass++
		}
	}
	if pass < 40 {
		t.Errorf("after SetLimit, expected ~all pass, got %d/50", pass)
	}
}

func TestKeyed_SetLimitZeroDisables(t *testing.T) {
	k := NewKeyed(rate.Limit(1), 1)
	_ = k.Allow("x")
	k.SetLimit(0, 0)
	for i := 0; i < 100; i++ {
		if !k.Allow("x") {
			t.Fatalf("rps=0 should allow all, denied at %d", i)
		}
	}
}

func TestKeyed_TimeRefillRestores(t *testing.T) {
	k := NewKeyed(rate.Limit(10), 2)
	_ = k.Allow("k1") // 2/2
	_ = k.Allow("k1") // 1/2
	_ = k.Allow("k1") // 0
	// 等 ~150ms,应至少恢复 1 token (10 RPS = 100ms/token)
	time.Sleep(150 * time.Millisecond)
	if !k.Allow("k1") {
		t.Errorf("after wait, should have refilled token")
	}
}

func TestKeyed_DifferentKeyTypesAreIsolated(t *testing.T) {
	k := NewKeyed(rate.Limit(2), 2)
	// 模拟 "ip:..." 和 "merchant:..." 两种 key prefix
	keys := []string{"ip:1.2.3.4", "merchant:m_alice", "ip:5.6.7.8"}
	for _, key := range keys {
		_ = k.Allow(key) // 各自吃 1
		_ = k.Allow(key) // 各自吃 2
		if k.Allow(key) {
			t.Errorf("%s should be exhausted", key)
		}
	}
	if k.Len() != 3 {
		t.Errorf("expect 3 keys, got %d", k.Len())
	}
}

func TestKeyed_EmptyKeyAllowed(t *testing.T) {
	k := NewKeyed(rate.Limit(10), 5)
	if !k.Allow("") {
		t.Error("empty key should still work (own bucket)")
	}
}

func TestKeyed_VeryHighRateNeverBlocks(t *testing.T) {
	k := NewKeyed(rate.Limit(1e6), 1000)
	for i := 0; i < 500; i++ {
		if !k.Allow("k") {
			t.Fatalf("high-rate limiter denied at %d", i)
		}
	}
}

func TestKeyed_RaceMixedKeys(t *testing.T) {
	k := NewKeyed(rate.Limit(1000), 100)
	var wg sync.WaitGroup
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(wid int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_ = k.Allow("worker-" + string(rune('A'+wid%26)))
			}
		}(w)
	}
	wg.Wait()
	// 不 panic 即过;Len 应当 ≤ 26
	if l := k.Len(); l > 26 {
		t.Errorf("Len=%d larger than expected key set", l)
	}
}

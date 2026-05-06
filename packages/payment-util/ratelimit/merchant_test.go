package ratelimit

import (
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/time/rate"
)

func TestMerchantLimiter_Allow(t *testing.T) {
	cfg := &MerchantConfig{
		Default: LimitSpec{RPS: 10, Burst: 20},
		Merchants: map[string]LimitSpec{
			"M_premium": {RPS: 100, Burst: 200},
			"M_test":    {RPS: 1, Burst: 2},
		},
	}

	throttled := &atomic.Int32{}
	limiter := NewMerchantLimiter(cfg, nil, func(merchantID string) {
		throttled.Add(1)
	})

	// 1. 空 merchant_id → 不限流
	if !limiter.Allow("") {
		t.Fatal("empty merchant_id should always allow")
	}

	// 2. premium merchant 应高额度
	allowed := 0
	for i := 0; i < 150; i++ {
		if limiter.Allow("M_premium") {
			allowed++
		}
	}
	if allowed < 100 {
		t.Fatalf("M_premium should allow ~100, got %d", allowed)
	}

	// 3. test merchant 应低额度
	allowed = 0
	throttled.Store(0)
	for i := 0; i < 50; i++ {
		if limiter.Allow("M_test") {
			allowed++
		}
	}
	if throttled.Load() <= 0 {
		t.Fatal("M_test should trigger throttling")
	}

	// 4. default merchant
	allowed = 0
	throttled.Store(0)
	for i := 0; i < 50; i++ {
		if limiter.Allow("M_default") {
			allowed++
		}
	}
	if throttled.Load() <= 0 {
		t.Fatal("M_default should trigger throttling")
	}
}

func TestMerchantLimiter_UpdateConfig(t *testing.T) {
	cfg1 := &MerchantConfig{
		Default: LimitSpec{RPS: 10, Burst: 20},
	}
	limiter := NewMerchantLimiter(cfg1, nil, nil)

	// 验证初始配置
	allowed := 0
	for i := 0; i < 50; i++ {
		if limiter.Allow("M1") {
			allowed++
		}
	}
	if allowed < 10 {
		t.Fatalf("initial config should allow ~10, got %d", allowed)
	}

	// 更新配置到更高的 rps
	cfg2 := &MerchantConfig{
		Default: LimitSpec{RPS: 100, Burst: 200},
	}
	limiter.UpdateConfig(cfg2)

	// 验证新配置
	allowed = 0
	for i := 0; i < 150; i++ {
		if limiter.Allow("M2") {
			allowed++
		}
	}
	if allowed < 100 {
		t.Fatalf("updated config should allow ~100, got %d", allowed)
	}
}

func TestMerchantLimiter_Concurrent(t *testing.T) {
	cfg := &MerchantConfig{
		Default: LimitSpec{RPS: 1000, Burst: 2000},
	}
	limiter := NewMerchantLimiter(cfg, nil, nil)

	// 并发 100 个 goroutine，每个对不同 merchant 做 1000 次 Allow
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			merchantID := "M" + string(rune(idx%10+'0'))
			for j := 0; j < 1000; j++ {
				limiter.Allow(merchantID)
			}
		}(i)
	}
	wg.Wait()

	// 应该没有 panic，且 limiter 能工作
	if !limiter.Allow("M_final") {
		t.Fatal("limiter should still work after concurrent access")
	}
}

func BenchmarkMerchantLimiter_Allow(b *testing.B) {
	cfg := &MerchantConfig{
		Default: LimitSpec{RPS: 1000, Burst: 2000},
		Merchants: map[string]LimitSpec{
			"M_premium": {RPS: 10000, Burst: 20000},
		},
	}
	limiter := NewMerchantLimiter(cfg, nil, nil)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		merchant := "M_" + string(rune((i%100)+'0'))
		limiter.Allow(merchant)
	}
}

func TestKeyedRateLimiter_Allow(t *testing.T) {
	limiter := NewKeyed(rate.Limit(10), 20)

	// 取 10 个 token（应该都通过）
	for i := 0; i < 10; i++ {
		if !limiter.Allow("key1") {
			t.Fatalf("should allow token %d", i)
		}
	}

	// 第 11 个应该被限（token 耗尽）
	if limiter.Allow("key1") {
		t.Fatal("should reject 11th token")
	}

	// 不同 key 应该独立
	if !limiter.Allow("key2") {
		t.Fatal("different key should have independent tokens")
	}
}

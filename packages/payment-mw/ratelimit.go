// ratelimit.go — token bucket 限流（per-IP / per-actor）。
//
// 策略:
//   - Per-IP: 防 unauthenticated 攻击
//   - Per-actor: 鉴权后按 merchant_id / ops_email 限流
//   - 全局 capacity / refill rate 可配
//
// 实现：sync.Map 存每个 key 的 token bucket（轻量；千万级 key 换 Redis）。
//
// 算法（leaky bucket / token bucket）：
//   每个 key 有 (tokens, last_refill) 状态
//   请求来时: tokens = min(capacity, tokens + (now - last_refill) * rate)
//   tokens >= 1 → 通过，tokens -= 1
//   否则 → 429

package mw

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimitConfig 限流配置。
type RateLimitConfig struct {
	// PerIPCapacity / PerIPRate: 按客户端 IP 限流（防 unauthenticated 攻击）
	PerIPCapacity int     // bucket 上限 (默认 100)
	PerIPRate     float64 // 每秒补充 (默认 10/s)

	// PerActorCapacity / PerActorRate: 鉴权后按 actor 限流
	PerActorCapacity int     // 默认 1000
	PerActorRate     float64 // 默认 100/s

	// KeyFn 自定义 key 提取（默认按 actor → IP → "anonymous"）
	KeyFn func(r *http.Request) string
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	mu         sync.Mutex
}

// RateLimit 中间件。
func RateLimit(cfg RateLimitConfig) Middleware {
	if cfg.PerIPCapacity == 0 {
		cfg.PerIPCapacity = 100
	}
	if cfg.PerIPRate == 0 {
		cfg.PerIPRate = 10
	}
	if cfg.PerActorCapacity == 0 {
		cfg.PerActorCapacity = 1000
	}
	if cfg.PerActorRate == 0 {
		cfg.PerActorRate = 100
	}
	if cfg.KeyFn == nil {
		cfg.KeyFn = defaultKey
	}
	buckets := &sync.Map{}
	// 后台 GC 清掉长期 idle 的 bucket（防内存泄漏）
	go gcLoop(buckets)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := cfg.KeyFn(r)
			// 鉴权过的 actor 走更高 capacity
			actor := ActorFromCtx(r.Context())
			cap := cfg.PerIPCapacity
			rate := cfg.PerIPRate
			if actor.Type != "anonymous" {
				cap = cfg.PerActorCapacity
				rate = cfg.PerActorRate
			}
			b := getBucket(buckets, key, cap)
			b.mu.Lock()
			now := time.Now()
			elapsed := now.Sub(b.lastRefill).Seconds()
			b.tokens = min64(float64(cap), b.tokens+elapsed*rate)
			b.lastRefill = now
			if b.tokens < 1 {
				retryAfter := int((1 - b.tokens) / rate)
				if retryAfter < 1 {
					retryAfter = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				w.Header().Set("X-RateLimit-Limit", strconv.Itoa(cap))
				w.Header().Set("X-RateLimit-Remaining", "0")
				b.mu.Unlock()
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			b.tokens--
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(cap))
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(int(b.tokens)))
			b.mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
}

func getBucket(m *sync.Map, key string, cap int) *bucket {
	if v, ok := m.Load(key); ok {
		return v.(*bucket)
	}
	b := &bucket{tokens: float64(cap), lastRefill: time.Now()}
	actual, _ := m.LoadOrStore(key, b)
	return actual.(*bucket)
}

// gcLoop 每 5min 清掉 10min 没 hit 的 bucket（防百万 IP 攻击撑爆内存）。
func gcLoop(m *sync.Map) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-10 * time.Minute)
		m.Range(func(k, v any) bool {
			b := v.(*bucket)
			b.mu.Lock()
			idle := b.lastRefill.Before(cutoff)
			b.mu.Unlock()
			if idle {
				m.Delete(k)
			}
			return true
		})
	}
}

func defaultKey(r *http.Request) string {
	actor := ActorFromCtx(r.Context())
	switch actor.Type {
	case "merchant":
		return "m:" + actor.MerchantID
	case "ops":
		return "o:" + actor.OpsEmail
	case "internal":
		return "i:internal"
	}
	// IP 兜底
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return "ip:" + xff
	}
	return "ip:" + r.RemoteAddr
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// Package ratelimit — token endpoint 单实例限流器。
//
// 防止:
//   - client_secret brute force (同 client_id 每秒>N 次 → 429)
//   - 单 IP 暴力扫描 (同 IP 全局每秒>N → 429)
//
// 算法: token bucket per key。
//
// 注: 多实例部署用 Redis 分布式限流（这里单实例够用，DDOS 应该顶 ingress 层）。

package ratelimit

import (
	"sync"
	"time"
)

// Limiter token bucket per key。
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64       // tokens per second
	burst   int           // bucket size
	ttl     time.Duration // 不活跃 key 清掉
}

type bucket struct {
	tokens   float64
	last     time.Time
	accessed time.Time
}

// New 构造。每秒 rate 个，bucket 容量 burst。
func New(rate float64, burst int) *Limiter {
	l := &Limiter{
		buckets: map[string]*bucket{},
		rate:    rate,
		burst:   burst,
		ttl:     10 * time.Minute,
	}
	go l.gcLoop()
	return l
}

// Allow 取 1 token，true=过。
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(l.burst), last: now}
		l.buckets[key] = b
	}
	// 补 token
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * l.rate
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.last = now
	b.accessed = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gcLoop 周期清不活跃 key (防内存膨胀)。
func (l *Limiter) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		l.gc()
	}
}

func (l *Limiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-l.ttl)
	for k, b := range l.buckets {
		if b.accessed.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// Size 当前管理的 key 数 (监控用)。
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// rate_per_ip.go: per-IP token bucket rate limiter.
//
// 用途：admin HTTP 端点防 DoS / brute-force（admin token 暴力枚举）。
// 跟现有 reliability.MerchantLimiter 区别：
//   - MerchantLimiter 是 gRPC 主路径，按 merchant_id；正常流量 500-5000 RPS
//   - IPLimiter 是 admin HTTP，按 client IP；正常流量 < 1 RPS（运营手动操作）
//
// 限制策略：默认 30 RPS / 60 burst per IP（远超人类操作 + 提供 dashboard
// 自动刷新空间）。超过 → 429 Too Many Requests。
//
// 实现：标准 golang.org/x/time/rate.Limiter (token bucket)；本包用同样的
// 算法但避免新增依赖（risk-manage 已经引入 reliability.MerchantLimiter
// 是自己手写的相同逻辑，所以这里复用之）。
package safety

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IPLimiter per-IP token bucket。
type IPLimiter struct {
	mu       sync.Mutex
	rps      float64
	burst    float64
	buckets  map[string]*ipBucket
	lastGC   time.Time
}

type ipBucket struct {
	tokens   float64
	lastFill time.Time
}

// NewIPLimiter rps 缺省 30；burst 缺省 60。
func NewIPLimiter(rps, burst float64) *IPLimiter {
	if rps <= 0 {
		rps = 30
	}
	if burst <= 0 {
		burst = 60
	}
	return &IPLimiter{
		rps:     rps,
		burst:   burst,
		buckets: make(map[string]*ipBucket, 256),
		lastGC:  time.Now(),
	}
}

// Allow 看 IP 是否可放行；不允许 → false（caller 返 429）。
func (l *IPLimiter) Allow(ip string) bool {
	if l == nil || ip == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[ip]
	if !ok {
		// 首次见 → 满 burst 给一次机会
		l.buckets[ip] = &ipBucket{tokens: l.burst - 1, lastFill: now}
		l.maybeGCLocked(now)
		return true
	}
	// refill
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens += elapsed * l.rps
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.lastFill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// maybeGCLocked 每 5min GC 一次：清掉 5min 内没活动的 IP（避免 map 无限
// 增长）。Locked = 调用方持锁。
func (l *IPLimiter) maybeGCLocked(now time.Time) {
	if now.Sub(l.lastGC) < 5*time.Minute {
		return
	}
	cutoff := now.Add(-5 * time.Minute)
	for ip, b := range l.buckets {
		if b.lastFill.Before(cutoff) {
			delete(l.buckets, ip)
		}
	}
	l.lastGC = now
}

// Size 当前活跃 IP 数（metric 用）。
func (l *IPLimiter) Size() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// IPRateLimit middleware：按 r.RemoteAddr (剔端口) 限。配 X-Forwarded-For
// 头时取首条。limiter nil → 等价 NoOp（dev / 单测）。
func IPRateLimit(limiter *IPLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limiter == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			if !limiter.Allow(ip) {
				w.Header().Set("Retry-After", "1")
				http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP 从 X-Forwarded-For / X-Real-IP / RemoteAddr 取，剔端口。
// 生产部署 nginx / api-gateway 必须 set X-Forwarded-For 让 IP 准确。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 取第一个（最远端 = 真实 client）
		if idx := strings.Index(xff, ","); idx > 0 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	if xrip := r.Header.Get("X-Real-IP"); xrip != "" {
		return strings.TrimSpace(xrip)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

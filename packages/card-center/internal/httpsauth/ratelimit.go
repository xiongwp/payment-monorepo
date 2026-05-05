// ratelimit.go: 防刷卡的 per-user rate limit。
//
// 攻击场景（card testing / BIN attack）：
//   - 攻击者拿到一个 user 账号 → 不停往 /v1/cards POST 不同 PAN
//   - 每次都让 card-center 调 KMS / 写 DB / 发 audit
//   - 1 分钟可能撞库几百张卡
//
// 防御：per-user token bucket，5 tokenize / 分钟 (默认)。超限 → 429 + Retry-After。
//
// 注意纪律：
//   - 限流窗口只针对 HTTPS 入口的 tokenize；mTLS gRPC 内部接口（card-center 与
//     order-core / api-gateway 之间）不限流，那是受信内部调用
//   - 限流 key = user_id（而非 IP）；同一用户跨设备共用一个桶
//   - 超限审计落 audit_log（reason = rate_limited）；运营可以看到行为
//   - 内存实现：单实例。多实例部署需要换成 Redis（接口已抽象，换实现即可）
package httpsauth

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RateLimiter per-user token bucket
type RateLimiter interface {
	// Allow 返 (允许, 剩余token, 下次补充时间)
	Allow(userID int64) (allowed bool, remaining int, retryAfter time.Duration)
}

// MemoryBucket 内存 token bucket。
//
// 算法：每个 user_id 维护一个 bucket，初始 burst 个 token。每次请求消费 1。
// 每 refillEvery 时间补 1 token，最多到 burst。
type MemoryBucket struct {
	burst       int           // 桶容量 (e.g. 5)
	refillEvery time.Duration // 多久补 1 个 token (e.g. 12s = 5/分钟)

	mu      sync.Mutex
	buckets map[int64]*userBucket
}

type userBucket struct {
	tokens     int
	lastRefill time.Time
}

// NewMemoryBucket 构造。dev 默认 5/分钟。
func NewMemoryBucket(burst int, refillEvery time.Duration) *MemoryBucket {
	if burst <= 0 {
		burst = 5
	}
	if refillEvery <= 0 {
		refillEvery = 12 * time.Second
	}
	return &MemoryBucket{
		burst:       burst,
		refillEvery: refillEvery,
		buckets:     make(map[int64]*userBucket),
	}
}

// Allow 消费 1 个 token；超限返 false。
func (m *MemoryBucket) Allow(userID int64) (bool, int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	b, ok := m.buckets[userID]
	if !ok {
		b = &userBucket{tokens: m.burst, lastRefill: now}
		m.buckets[userID] = b
	}
	// 按时间补 token
	elapsed := now.Sub(b.lastRefill)
	if elapsed >= m.refillEvery {
		add := int(elapsed / m.refillEvery)
		b.tokens += add
		if b.tokens > m.burst {
			b.tokens = m.burst
		}
		// lastRefill 推进到对齐边界，避免漂移
		b.lastRefill = b.lastRefill.Add(time.Duration(add) * m.refillEvery)
	}
	if b.tokens <= 0 {
		// 下次能补的时间
		next := b.lastRefill.Add(m.refillEvery).Sub(now)
		if next < 0 {
			next = m.refillEvery
		}
		return false, 0, next
	}
	b.tokens--
	return true, b.tokens, 0
}

// Cleanup 周期清理 1h 没动过的 bucket（防内存膨胀）。
//
// 启动一个 goroutine 定期清；fx 不接管 goroutine 退出，因为 process 死时整个 GC
// 都收回。
func (m *MemoryBucket) Cleanup(every time.Duration, idle time.Duration) {
	if every <= 0 {
		every = 10 * time.Minute
	}
	if idle <= 0 {
		idle = time.Hour
	}
	ticker := time.NewTicker(every)
	go func() {
		for range ticker.C {
			m.mu.Lock()
			cutoff := time.Now().Add(-idle)
			for k, b := range m.buckets {
				if b.lastRefill.Before(cutoff) && b.tokens >= m.burst {
					delete(m.buckets, k)
				}
			}
			m.mu.Unlock()
		}
	}()
}

// RateLimitMiddleware 给 tokenize 路径用。
//
// 必须套在 auth middleware 之后（要 ctx.user_id）。listCards / deleteCard 不套
// 限流（频次低、风险低）。
func RateLimitMiddleware(rl RateLimiter, logger *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid, ok := UserIDFromCtx(r.Context())
			if !ok {
				// 没 user_id → auth middleware 应该已经拦了；这里兜底拒
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no user"})
				return
			}
			allowed, remaining, retry := rl.Allow(uid)
			w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
			if !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				logger.Warn("tokenize rate limited",
					zap.Int64("user_id", uid),
					zap.Duration("retry_after", retry))
				writeJSON(w, http.StatusTooManyRequests, map[string]string{
					"error": "rate limited: too many card binding attempts",
				})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

package store

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// CachedCounter 在 Counter 上加薄一层 in-process 缓存，把 hot path 重复读合并。
//
// 动机：单笔 Screen 通常会跑 30+ 规则；其中 amount_limit / velocity /
// velocity_amount / rolling_amount 等多条规则用同样的 (key, dim) 组合调
// counter（例如 amount_limit 查 GetDaily/GetMonthly，rolling_amount 查
// GetRollingDays，全是 customer:X scope）。每次都打 Redis 是浪费。
//
// 缓存语义：
//   - per (method, key, arg) memoize；TTL 短（默认 1s），过期穿透回底层
//   - Incr 失效：当 key 写入时，把所有以该 key 起头的 entry 清掉，避免脏读
//   - 进程内 sync.Map；不跨进程共享（跨进程缓存留给 featurecache 包做）
//
// 默认 TTL 1s 在 Screen 一次完整调用栈（< 100ms）内远远够用，跨调用粒度也
// 能挡掉 burst 流量下的同 key 重复请求。可调大或调小走 NewCachedCounter。
type CachedCounter struct {
	inner Counter
	ttl   time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry

	// 命中率统计：原子计数让 metric 直接读取无锁。给 Prometheus
	// risk_counter_cache_hit_total / risk_counter_cache_miss_total 用。
	// hot path 极致优化 — 用 atomic 而不是 mutex protect（hit metric 落
	// 后几条不影响功能正确性）。
	hits   uint64
	misses uint64
}

type cacheEntry struct {
	val       int64
	expiresAt time.Time
}

// NewCachedCounter wraps an underlying Counter. ttl <= 0 disables caching
// (passes through to inner)，方便单测。
func NewCachedCounter(inner Counter, ttl time.Duration) *CachedCounter {
	return &CachedCounter{inner: inner, ttl: ttl, cache: make(map[string]cacheEntry, 1024)}
}

func (c *CachedCounter) cacheKey(method, key string, arg int) string {
	return method + "|" + key + "|" + strconv.Itoa(arg)
}

func (c *CachedCounter) get(k string) (int64, bool) {
	if c.ttl <= 0 {
		atomic.AddUint64(&c.misses, 1)
		return 0, false
	}
	c.mu.RLock()
	e, ok := c.cache[k]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		atomic.AddUint64(&c.misses, 1)
		return 0, false
	}
	atomic.AddUint64(&c.hits, 1)
	return e.val, true
}

// CacheStats 命中率统计。给 metric / dashboard 读。
type CacheStats struct {
	Hits   uint64
	Misses uint64
}

// CacheStats 当前累计 hit / miss 计数。
func (c *CachedCounter) CacheStats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	return CacheStats{
		Hits:   atomic.LoadUint64(&c.hits),
		Misses: atomic.LoadUint64(&c.misses),
	}
}

// CacheSize 当前 cache 字典大小（entries）。给 metric 看 OOM 压力。
func (c *CachedCounter) CacheSize() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func (c *CachedCounter) set(k string, v int64) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.cache[k] = cacheEntry{val: v, expiresAt: time.Now().Add(c.ttl)}
	// 简单容量保护：超过 4096 entry 清一半（最旧的，依赖 map 迭代顺序近似）。
	// 风控负载下单进程命中的 (key,scope) 一般 < 1k；这只是 OOM 兜底。
	if len(c.cache) > 4096 {
		i := 0
		for k := range c.cache {
			delete(c.cache, k)
			i++
			if i >= 2048 {
				break
			}
		}
	}
	c.mu.Unlock()
}

// invalidate 清掉所有 method 下含 key 的 entry。Incr 时调，让接下来的 Get
// 看到最新值（虽然底层 Counter 本身的延迟一致性可能更弱，但至少消除"刚 Incr
// 完 Get 仍读旧"这种最常见踩坑）。
func (c *CachedCounter) invalidate(key string) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	for k := range c.cache {
		// k = "method|key|arg"；找第一个 "|" 后到第二个 "|" 之间是 key。
		first := -1
		second := -1
		for i := 0; i < len(k); i++ {
			if k[i] == '|' {
				if first < 0 {
					first = i
				} else {
					second = i
					break
				}
			}
		}
		if first < 0 || second < 0 {
			continue
		}
		if k[first+1:second] == key {
			delete(c.cache, k)
		}
	}
	c.mu.Unlock()
}

func (c *CachedCounter) GetDaily(ctx context.Context, key string) int64 {
	ck := c.cacheKey("daily", key, 0)
	if v, ok := c.get(ck); ok {
		return v
	}
	v := c.inner.GetDaily(ctx, key)
	c.set(ck, v)
	return v
}

func (c *CachedCounter) GetMonthly(ctx context.Context, key string) int64 {
	ck := c.cacheKey("monthly", key, 0)
	if v, ok := c.get(ck); ok {
		return v
	}
	v := c.inner.GetMonthly(ctx, key)
	c.set(ck, v)
	return v
}

func (c *CachedCounter) GetVelocity(ctx context.Context, key string, windowMin int) int {
	ck := c.cacheKey("velocity", key, windowMin)
	if v, ok := c.get(ck); ok {
		return int(v)
	}
	v := c.inner.GetVelocity(ctx, key, windowMin)
	c.set(ck, int64(v))
	return v
}

func (c *CachedCounter) GetVelocityAmount(ctx context.Context, key string, windowMin int) int64 {
	ck := c.cacheKey("velamt", key, windowMin)
	if v, ok := c.get(ck); ok {
		return v
	}
	v := c.inner.GetVelocityAmount(ctx, key, windowMin)
	c.set(ck, v)
	return v
}

func (c *CachedCounter) GetRollingDays(ctx context.Context, key string, days int) int64 {
	ck := c.cacheKey("rolling", key, days)
	if v, ok := c.get(ck); ok {
		return v
	}
	v := c.inner.GetRollingDays(ctx, key, days)
	c.set(ck, v)
	return v
}

func (c *CachedCounter) Incr(ctx context.Context, key string, amount int64) {
	c.inner.Incr(ctx, key, amount)
	c.invalidate(key)
}

func (c *CachedCounter) Purge(ctx context.Context, key string) int {
	n := c.inner.Purge(ctx, key)
	c.invalidate(key)
	return n
}

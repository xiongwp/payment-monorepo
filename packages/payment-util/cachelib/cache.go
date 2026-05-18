// Package cachelib provides a service-shared cache abstraction with
// pluggable backends (in-memory, Redis) plus read-through + singleflight
// patterns to prevent cache stampede.
//
// Design goals:
//   - Backend-agnostic interface (`Cache`) → 业务代码不需要直接依赖 go-redis.
//   - Each consumer service can inject any client that satisfies the tiny
//     `RedisClient` adapter interface — payment-util stays redis-free, but
//     services can plug in go-redis / rueidis / fake client for tests.
//   - Built-in `ReadThrough` helper with singleflight: 同一 key 并发未命中
//     时只放过一个 loader 调用, 其他等结果.
//   - Optional value codec (e.g. JSON / msgpack); 默认 raw []byte.
//
// 典型使用:
//
//	cache := cachelib.NewRedis(redisAdapter{cli}, "order-core:pi", 5*time.Minute)
//	// 或 dev/单测:
//	cache := cachelib.NewMemory(5*time.Minute)
//
//	pi, err := cachelib.ReadThrough(ctx, cache, "pi:"+id, 5*time.Minute,
//	    func(ctx context.Context) (*PaymentIntent, error) {
//	        return repo.Get(ctx, id)
//	    })
package cachelib

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound 是 Cache.Get 在 miss 时可以返回的 sentinel (也可以返 ok=false).
// 大多数实现走 ok=false, 留 ErrNotFound 给 codec / loader 中转用.
var ErrNotFound = errors.New("cache miss")

// Cache 通用缓存接口. 所有方法应在合理超时内完成 (调用方传 ctx).
//
// Get 语义:
//   - ok=true  → 命中, val 是缓存值 (可能是 nil/空 byte slice 表示 "已知不存在").
//   - ok=false → 未命中, val 应忽略.
//   - err 非 nil → 后端异常 (e.g. Redis timeout); 调用方应当作 miss + 告警.
type Cache interface {
	Get(ctx context.Context, key string) (val []byte, ok bool, err error)
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
}

// RedisClient 是 cachelib 期望的最小 Redis 命令集. 任何 go-redis client 都可以
// 通过一个 ~30 行 adapter 满足这个接口, 避免 payment-util 直接依赖 go-redis.
//
// 实现示例 (在 consumer 包):
//
//	type redisGoAdapter struct{ c *redis.Client }
//	func (a redisGoAdapter) Get(ctx context.Context, k string) (string, error) {
//	    v, err := a.c.Get(ctx, k).Result()
//	    if errors.Is(err, redis.Nil) { return "", cachelib.ErrNotFound }
//	    return v, err
//	}
//	// Set / Del 同理...
type RedisClient interface {
	// Get 返 (value, err). Miss 返 ("", ErrNotFound).
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
	Del(ctx context.Context, keys ...string) error
}

// ─── Memory backend (dev / fallback) ────────────────────────────────────

// memoryCache 进程内 LRU-ish 缓存. 默认 TTL 由构造器固定; Set 时可覆盖.
//
// 不做严格 LRU 容量限制 (生产建议 Redis); 只是 TTL-driven 自动过期, 后台 GC.
type memoryCache struct {
	mu         sync.RWMutex
	m          map[string]*memEntry
	defaultTTL time.Duration
}

type memEntry struct {
	val       []byte
	expiresAt time.Time
}

// NewMemory 构造内存缓存. defaultTTL <=0 → 不过期 (调用方主动 Del 清).
func NewMemory(defaultTTL time.Duration) Cache {
	c := &memoryCache{
		m:          make(map[string]*memEntry),
		defaultTTL: defaultTTL,
	}
	go c.gcLoop()
	return c
}

func (c *memoryCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false, nil
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return nil, false, nil
	}
	// 返副本防外部改动
	out := make([]byte, len(e.val))
	copy(out, e.val)
	return out, true, nil
}

func (c *memoryCache) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	stored := make([]byte, len(val))
	copy(stored, val)
	c.mu.Lock()
	c.m[key] = &memEntry{val: stored, expiresAt: expiresAt}
	c.mu.Unlock()
	return nil
}

func (c *memoryCache) Del(_ context.Context, keys ...string) error {
	c.mu.Lock()
	for _, k := range keys {
		delete(c.m, k)
	}
	c.mu.Unlock()
	return nil
}

func (c *memoryCache) gcLoop() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		c.gc()
	}
}

func (c *memoryCache) gc() {
	now := time.Now()
	c.mu.Lock()
	for k, e := range c.m {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// ─── Redis backend ──────────────────────────────────────────────────────

type redisCache struct {
	cli        RedisClient
	prefix     string
	defaultTTL time.Duration
}

// NewRedis 构造 Redis 后端缓存. prefix 为业务 namespace (会拼到 key 前避免不同
// 服务撞 key), defaultTTL 为缺省过期时间; Set 显式传 ttl 时覆盖.
func NewRedis(cli RedisClient, prefix string, defaultTTL time.Duration) Cache {
	if !endsWithColon(prefix) && prefix != "" {
		prefix += ":"
	}
	return &redisCache{cli: cli, prefix: prefix, defaultTTL: defaultTTL}
}

func (c *redisCache) full(k string) string { return c.prefix + k }

func (c *redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := c.cli.Get(ctx, c.full(key))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(v), true, nil
}

func (c *redisCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	return c.cli.Set(ctx, c.full(key), string(val), ttl)
}

func (c *redisCache) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	full := make([]string, len(keys))
	for i, k := range keys {
		full[i] = c.full(k)
	}
	return c.cli.Del(ctx, full...)
}

func endsWithColon(s string) bool { return len(s) > 0 && s[len(s)-1] == ':' }

// ─── Noop backend (test convenience) ────────────────────────────────────

type noopCache struct{}

// NewNoop 总是 miss 的缓存; 测试 / 显式禁用缓存路径用.
func NewNoop() Cache { return noopCache{} }

func (noopCache) Get(_ context.Context, _ string) ([]byte, bool, error) { return nil, false, nil }
func (noopCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (noopCache) Del(_ context.Context, _ ...string) error { return nil }

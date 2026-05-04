// Package featurecache —— 预计算特征缓存。
//
// 用途：把 hot path 上要查的"派生特征"（30d 累计 / IP 反查 / 设备指纹相似度
// 等）从 Screen 路径剥离，由后台 worker 异步预计算，Engine 直接读缓存。
//
// 适用场景：
//
//   - 特征计算重（例如图查询 LinkStore.Fanout）但变化慢（用户图边天级别更新）
//   - 同 key 在短时间内被多次 Screen 命中（瞬时拍单 / 接口频调）
//   - 对一致性容忍 N 秒延迟（risk 决策对绝对实时性要求弱于支付路径）
//
// 不适用：
//
//   - 极强一致需求（黑名单命中要立即生效 → 走 store.Blacklist 直读）
//   - 单次性的随机 query（cache miss 率 100% 时纯 overhead）
//
// 接口设计：Get/Set value 是 []byte，调用方自己 marshal（JSON / protobuf 都行）；
// 让本包不耦合具体特征 schema。
//
// 实现：
//   - MemCache: sync.Map + TTL，单进程；进程内多副本会各自 cache（一致性弱）
//   - RedisCache: 跨进程共享；TTL 由 Redis SET EX 管
package featurecache

import (
	"context"
	"sync"
	"time"
)

// Cache 特征缓存抽象。
type Cache interface {
	// Get 读 key 对应特征 blob；不存在 / 过期返回 (nil, false)。
	Get(ctx context.Context, key string) ([]byte, bool)
	// Set 写入；ttl <= 0 用实现侧默认（Mem 5min，Redis 5min）。
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
	// Delete 失效（key 对应数据更新时调）。
	Delete(ctx context.Context, key string)
}

// ─── Mem 实现 ───────────────────────────────────────────────────────────────

// MemCache 进程内 cache。重启清零。多副本不共享。
type MemCache struct {
	defaultTTL time.Duration
	mu         sync.RWMutex
	m          map[string]memEntry
}

type memEntry struct {
	value     []byte
	expiresAt time.Time
}

func NewMemCache(defaultTTL time.Duration) *MemCache {
	if defaultTTL <= 0 {
		defaultTTL = 5 * time.Minute
	}
	return &MemCache{defaultTTL: defaultTTL, m: make(map[string]memEntry, 256)}
}

func (c *MemCache) Get(_ context.Context, key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := c.m[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	// 拷贝出 slice 防外部改动写到 cache 里
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, true
}

func (c *MemCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) {
	if ttl <= 0 {
		ttl = c.defaultTTL
	}
	stored := make([]byte, len(value))
	copy(stored, value)
	c.mu.Lock()
	c.m[key] = memEntry{value: stored, expiresAt: time.Now().Add(ttl)}
	// 容量兜底
	if len(c.m) > 8192 {
		i := 0
		for k := range c.m {
			delete(c.m, k)
			i++
			if i >= 4096 {
				break
			}
		}
	}
	c.mu.Unlock()
}

func (c *MemCache) Delete(_ context.Context, key string) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// ─── Noop 实现（关缓存时用） ────────────────────────────────────────────────

type NoopCache struct{}

func (NoopCache) Get(context.Context, string) ([]byte, bool)        { return nil, false }
func (NoopCache) Set(context.Context, string, []byte, time.Duration) {}
func (NoopCache) Delete(context.Context, string)                     {}

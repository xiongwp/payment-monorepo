// idempotency.go: Screen 调用幂等缓存。
//
// 商业部署 payment-core 经常会因网络重试、双数据中心切流等原因对同一笔
// 交易触发多次 Screen。如果不去重：
//   - audit 表里同一交易出现 N 条决策记录，污染下游 ML 训练
//   - review queue push N 次（虽然 MemStore 内幂等，但 PG 实现可能不一定）
//   - 频控 / 图谱副作用被多次累计（这些走 Report，不在 Screen 里，但 ML
//     score 多次调用浪费 GPU 配额）
//
// 入参 TxnContext.IdempotencyKey 非空时启用：在 window 内同 key 第二次
// 调用直接返回 cache 里的 Result，不再 evaluate。
//
// 实现：内存 LRU + TTL（默认 60s + 10k 条上限）；生产可换 Redis（同 key
// 跨实例去重，但延迟代价可接受 — 1 RTT < 1ms）。

package service

import (
	"sync"
	"time"

	"github.com/xiongwp/risk-manage/internal/engine"
)

const (
	defaultIdempotencyTTL  = 60 * time.Second
	defaultIdempotencyMax  = 10_000
)

type idempotencyEntry struct {
	res    *engine.Result
	expire time.Time
}

// idempotencyCache 简单 TTL map + 容量上限（O(N) sweep on overflow，足够
// 10k 量级；生产换 Redis）。线程安全。
type idempotencyCache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	items map[string]idempotencyEntry
}

func newIdempotencyCache(ttl time.Duration, max int) *idempotencyCache {
	if ttl <= 0 {
		ttl = defaultIdempotencyTTL
	}
	if max <= 0 {
		max = defaultIdempotencyMax
	}
	return &idempotencyCache{
		ttl:   ttl,
		max:   max,
		items: make(map[string]idempotencyEntry, max/2),
	}
}

// Get 命中且未过期 → 返回 cached result。否则 nil（caller 走正常 evaluate）。
func (c *idempotencyCache) Get(key string) *engine.Result {
	if key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return nil
	}
	if time.Now().After(e.expire) {
		delete(c.items, key)
		return nil
	}
	return e.res
}

// Put 写入 cache。超容量时简单清掉过期 + 抽样删（O(N)，10k 规模 < 1ms）。
func (c *idempotencyCache) Put(key string, res *engine.Result) {
	if key == "" || res == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = idempotencyEntry{res: res, expire: time.Now().Add(c.ttl)}
	if len(c.items) <= c.max {
		return
	}
	// 简单淘汰：扫一遍把过期的都删；如果还满，再删除若干"最早过期"的项
	now := time.Now()
	for k, e := range c.items {
		if now.After(e.expire) {
			delete(c.items, k)
		}
	}
	// 仍超：删一半（生产用 LRU；当前简化）
	if len(c.items) > c.max {
		toDel := len(c.items) - c.max
		for k := range c.items {
			delete(c.items, k)
			toDel--
			if toDel <= 0 {
				break
			}
		}
	}
}

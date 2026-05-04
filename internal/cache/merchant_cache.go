// Package cache — in-process LRU + TTL merchant cache.
//
// Hot path: PaymentIntent.Create / Authenticate 每笔都按 (id | api-key-hash)
// 查 merchants 表。万级商户，走一次 DB 300µs+；放进 LRU 之后常态 < 1µs。
//
// 失效策略：Update / RotateAPIKeys / TransitionKYC 之后在 service 层调 Invalidate；
// TTL 是兜底（防止未覆盖的外部变更）。
//
// Observability: Get/Lookup 走计数器累加 hit/miss；调用方可通过 Prometheus 盯命中率
// 决定是否调大 size / ttl。
package cache

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// MetricsHook 每次缓存操作都会调。byID / byKeyHash 两个索引各打一条。
// 调用方传 nil 就退化成无指标模式（测试友好）。
type MetricsHook interface {
	// Lookup 在 Get/Lookup 之后；hit=true 计入 hit 桶，否则 miss。
	Lookup(index string, hit bool)
	// Size 更新当前条目数。
	Size(index string, n int)
}

const (
	defaultSize = 10_000
	defaultTTL  = 60 * time.Second
)

// MerchantCache 用 (id → *Merchant) 作为唯一数据源；(keyHash → id) 作为二级索引。
// 这样 Invalidate 只需要删 byID 一个入口，hashToID 的陈旧映射下次查 byID 会 miss
// 自动降级到 DB，不会返回过期商户。
type MerchantCache struct {
	byID     *expirable.LRU[string, *domain.Merchant]
	hashToID *expirable.LRU[string, string]
	metrics  MetricsHook
}

// New 构造。size/ttl 任一 <=0 走默认（10k / 60s）；metrics nil 允许。
func New(size int, ttl time.Duration, metrics MetricsHook) *MerchantCache {
	if size <= 0 {
		size = defaultSize
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return &MerchantCache{
		byID:     expirable.NewLRU[string, *domain.Merchant](size, nil, ttl),
		hashToID: expirable.NewLRU[string, string](size, nil, ttl),
		metrics:  metrics,
	}
}

// GetByID cache-aside 读；miss 时返回 (nil, false)。
func (c *MerchantCache) GetByID(id string) (*domain.Merchant, bool) {
	m, ok := c.byID.Get(id)
	c.track("by_id", ok)
	return m, ok
}

// LookupByKeyHash 通过 API key 的 SHA-256 hash 反查商户。
// hashToID 和 byID 都命中才算 hit；任一 miss 都返回 (nil, false) 让调用方走 DB。
func (c *MerchantCache) LookupByKeyHash(hash string) (*domain.Merchant, bool) {
	id, ok := c.hashToID.Get(hash)
	if !ok {
		c.track("by_key_hash", false)
		return nil, false
	}
	m, ok := c.byID.Get(id)
	c.track("by_key_hash", ok)
	return m, ok
}

// Put 把最新的 merchant 写入两个索引。live/test hash 为空就不写（不会覆盖）。
func (c *MerchantCache) Put(m *domain.Merchant) {
	if m == nil || m.ID == "" {
		return
	}
	c.byID.Add(m.ID, m)
	if m.LiveKeyHash != "" {
		c.hashToID.Add(m.LiveKeyHash, m.ID)
	}
	if m.TestKeyHash != "" {
		c.hashToID.Add(m.TestKeyHash, m.ID)
	}
	c.reportSize()
}

// Invalidate 删 byID 那一条；hashToID 的残留条目会在下次 byID miss 时自动降级到 DB，
// 读取后由 Put 覆盖为新值。不需要遍历 hashToID 找出来一并删。
func (c *MerchantCache) Invalidate(id string) {
	c.byID.Remove(id)
	c.reportSize()
}

// Purge 清空（测试用 + 管理端 reload 用）。
func (c *MerchantCache) Purge() {
	c.byID.Purge()
	c.hashToID.Purge()
	c.reportSize()
}

// Len 当前 byID / byKeyHash 的条目数，调用方（warmup / metrics 扫描）用。
func (c *MerchantCache) Len() (byID, byKeyHash int) {
	return c.byID.Len(), c.hashToID.Len()
}

func (c *MerchantCache) track(index string, hit bool) {
	if c.metrics != nil {
		c.metrics.Lookup(index, hit)
	}
}

func (c *MerchantCache) reportSize() {
	if c.metrics != nil {
		c.metrics.Size("by_id", c.byID.Len())
		c.metrics.Size("by_key_hash", c.hashToID.Len())
	}
}

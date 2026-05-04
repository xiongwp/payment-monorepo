// Package extsignal 外部反欺诈信号集成 (Sift / MaxMind / IPQS / Onfido / ...)。
//
// 风控决策可选 join 第三方分数（信用 / fraud reputation / IP intelligence
// 等）做规则触发。本包：
//   - 抽象 Provider 接口让 callers 不依赖具体厂商
//   - 提供 ScoreCache 给同 (entity, provider) key 短期缓存（避免每笔
//     交易都打外部 API + 控成本）
//   - HTTP webhook 入口让外部系统主动 push 分数（async，不阻塞 Screen）
//
// 主路径 Screen 不直接 call provider（外部 API 5-50ms；fail-open 时阻塞
// risk-manage SLA）。推荐流程：
//   1. payment-core 在 Screen 之前先 enrich：调 Sift/MaxMind 拿分 → 塞
//      txn.Metadata（"sift_score": "..."）
//   2. risk-manage 规则读 metadata 做判定（直接纯 in-process）
//   3. 同时跑外部 webhook 接口接收 push 分数 → 缓存给后续 query 用
//
// 当前包提供 stub Provider（永远返 ProviderResult{}，满足 interface）+
// ScoreCache + Webhook handler 框架；接 Sift/MaxMind 时换具体 provider impl。
package extsignal

import (
	"context"
	"sync"
	"time"
)

// ProviderName 第三方 ID 标识。
type ProviderName string

const (
	ProviderSift    ProviderName = "sift"
	ProviderMaxMind ProviderName = "maxmind"
	ProviderIPQS    ProviderName = "ipqs"
	ProviderOnfido  ProviderName = "onfido"
)

// EntityKey 实体类型 + key（如 "user:42" / "ip:1.2.3.4" / "card_bin:1234"）。
type EntityKey struct {
	Type string // user / ip / card_bin / email_domain / device
	Key  string
}

func (e EntityKey) String() string { return e.Type + ":" + e.Key }

// ProviderResult 外部分数标准化结果。
//
//	Score: 0.0 - 1.0；越高越可疑（保持跟内部 ml_score 一致语义）
//	Reasons: 厂商返回的命中类目 / 标签
//	RawJSON: 原始 JSON，给审计 / 后处理
//	FetchedAt: 缓存判过期用
type ProviderResult struct {
	Provider  ProviderName
	Entity    EntityKey
	Score     float64
	Reasons   []string
	RawJSON   string
	FetchedAt time.Time
}

// IsStale 缓存过期判断（Score 默认 60min 有效）。
func (r ProviderResult) IsStale(ttl time.Duration) bool {
	if r.FetchedAt.IsZero() {
		return true
	}
	return time.Since(r.FetchedAt) > ttl
}

// Provider 单个第三方的客户端接口。Lookup 是同步调；调用方应该套熔断器
// 保护主路径。
type Provider interface {
	Name() ProviderName
	Lookup(ctx context.Context, entity EntityKey) (ProviderResult, error)
}

// ScoreCache (entity, provider) → ProviderResult 短期缓存。控成本（按
// 厂商付费）+ 抗抖动。生产应该换 Redis 跨进程共享。
type ScoreCache struct {
	mu  sync.RWMutex
	ttl time.Duration
	m   map[string]ProviderResult
}

func NewScoreCache(ttl time.Duration) *ScoreCache {
	if ttl <= 0 {
		ttl = 60 * time.Minute
	}
	return &ScoreCache{ttl: ttl, m: make(map[string]ProviderResult)}
}

func cacheKey(p ProviderName, e EntityKey) string {
	return string(p) + "|" + e.String()
}

// Get 命中且未过期 → (result, true)；缺失 / 过期 → ("", false)。
func (c *ScoreCache) Get(p ProviderName, e EntityKey) (ProviderResult, bool) {
	if c == nil {
		return ProviderResult{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.m[cacheKey(p, e)]
	if !ok {
		return r, false
	}
	if r.IsStale(c.ttl) {
		return r, false
	}
	return r, true
}

// Put 写入 / 替换。FetchedAt 自动设 now（如果调用方没填）。
func (c *ScoreCache) Put(r ProviderResult) {
	if c == nil {
		return
	}
	if r.FetchedAt.IsZero() {
		r.FetchedAt = time.Now().UTC()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[cacheKey(r.Provider, r.Entity)] = r
}

// Purge 清掉指定 entity 所有 provider 的缓存（GDPR right-to-erasure）。
func (c *ScoreCache) Purge(e EntityKey) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for k := range c.m {
		if c.m[k].Entity == e {
			delete(c.m, k)
			n++
		}
	}
	return n
}

// Size 当前缓存条目数（metric / debug 用）。
func (c *ScoreCache) Size() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// CachedLookup wrapper：先查缓存，miss 走 provider。fail-open：provider
// 出错时返空 result + cache miss（让规则按 score=0 判，不阻断决策）。
func CachedLookup(ctx context.Context, c *ScoreCache, p Provider, e EntityKey) ProviderResult {
	if c != nil {
		if r, ok := c.Get(p.Name(), e); ok {
			return r
		}
	}
	r, err := p.Lookup(ctx, e)
	if err != nil {
		return ProviderResult{Provider: p.Name(), Entity: e}
	}
	r.Provider = p.Name()
	r.Entity = e
	if r.FetchedAt.IsZero() {
		r.FetchedAt = time.Now().UTC()
	}
	if c != nil {
		c.Put(r)
	}
	return r
}

// stubProvider 满足 Provider 接口但永远返空 result。给单测 / 缺省 wiring
// 用（不强制部署接 Sift / MaxMind）。
type stubProvider struct{ name ProviderName }

func NewStubProvider(name ProviderName) Provider          { return stubProvider{name: name} }
func (s stubProvider) Name() ProviderName                  { return s.name }
func (s stubProvider) Lookup(_ context.Context, _ EntityKey) (ProviderResult, error) {
	return ProviderResult{Provider: s.name}, nil
}

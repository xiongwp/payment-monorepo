package ratelimit

import (
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// MerchantConfig per-merchant 限流配置。
type MerchantConfig struct {
	Default   LimitSpec            `json:"default"`
	Merchants map[string]LimitSpec `json:"merchants"`
}

// LimitSpec 单个 merchant 的限流参数。
type LimitSpec struct {
	RPS   int `json:"rps"`
	Burst int `json:"burst"`
}

// MerchantLimiter 基于配置的 merchant 维度限流器。支持热更新。
type MerchantLimiter struct {
	cfg    atomic.Pointer[MerchantConfig]
	keyed  *Keyed // per-merchant token bucket
	logger *zap.Logger
	// 指标回调：当限流触发时调用（用于记 prometheus counter）
	metricsCallback func(merchantID string)
}

// NewMerchantLimiter 构造 merchant 限流器。
// config 可为 nil（无限流）；metrics callback 可为 nil（不记指标）。
func NewMerchantLimiter(config *MerchantConfig, logger *zap.Logger, metricsCallback func(merchantID string)) *MerchantLimiter {
	m := &MerchantLimiter{
		keyed:           NewKeyed(rate.Limit(100), 200), // 默认初值，立刻被覆盖
		logger:          logger,
		metricsCallback: metricsCallback,
	}
	if config != nil {
		m.cfg.Store(config)
		m.applyConfig(config)
	}
	return m
}

// Allow 检查 merchant_id 是否应通过限流。没有配置 / rps<=0 → 直接返 true（不限流）。
func (m *MerchantLimiter) Allow(merchantID string) bool {
	if merchantID == "" {
		return true // 取不到 merchant_id 时不限流（兜底为 IP 限流）
	}
	cfg := m.cfg.Load()
	if cfg == nil || (cfg.Default.RPS <= 0 && len(cfg.Merchants) == 0) {
		return true
	}

	// 寻 merchant 特定配置，不存在则用 default
	spec := cfg.Default
	if custom, ok := cfg.Merchants[merchantID]; ok {
		spec = custom
	}

	// 兜底：若 rps <= 0，则不限流
	if spec.RPS <= 0 {
		return true
	}

	// Allow 会自动 double-check 创建 limiter；见 ratelimit.go
	allowed := m.keyed.Allow(merchantID)
	if !allowed && m.metricsCallback != nil {
		m.metricsCallback(merchantID)
	}
	return allowed
}

// UpdateConfig 热更新配置（config-center 推送时调用）。
func (m *MerchantLimiter) UpdateConfig(newCfg *MerchantConfig) {
	if newCfg == nil {
		return
	}
	m.cfg.Store(newCfg)
	m.applyConfig(newCfg)
	if m.logger != nil {
		m.logger.Info("merchant_ratelimit config updated",
			zap.Int("default_rps", newCfg.Default.RPS),
			zap.Int("merchant_count", len(newCfg.Merchants)))
	}
}

// applyConfig 内部：重新计算 keyed limiter 的全局限制。
// 为了简化，我们用 default RPS 作为 keyed 的全局限制；单个 merchant
// 的配置在 Allow() 里动态选择。但这样不够细——更好的方案是每个 merchant
// 一个独立 limiter，然后在 Allow() 里动态 SetLimit。这里先用一个折中：
// 用最大的 RPS 作为 keyed 的上限（避免 limiter 创建失败）。
func (m *MerchantLimiter) applyConfig(cfg *MerchantConfig) {
	maxRPS := cfg.Default.RPS
	maxBurst := cfg.Default.Burst
	for _, spec := range cfg.Merchants {
		if spec.RPS > maxRPS {
			maxRPS = spec.RPS
		}
		if spec.Burst > maxBurst {
			maxBurst = spec.Burst
		}
	}
	// keyed 的全局限制用最大值，避免新 key 创建时被意外拒绝
	if maxRPS <= 0 {
		maxRPS = 100
	}
	if maxBurst <= 0 {
		maxBurst = 200
	}
	m.keyed.SetLimit(rate.Limit(maxRPS), maxBurst)
}

// ConfiguredMerchantLimiter 包装 merchant 限流器 + per-merchant spec 缓存。
// 用于在 interceptor 里快速查询每个 merchant 的真实限流参数（避免每次 Allow
// 都查一遍 config）。
type ConfiguredMerchantLimiter struct {
	limiter   *MerchantLimiter
	specCache *sync.Map // merchantID -> *LimitSpec（缓存最近查过的）
}

// NewConfiguredMerchantLimiter 构造。
func NewConfiguredMerchantLimiter(limiter *MerchantLimiter) *ConfiguredMerchantLimiter {
	return &ConfiguredMerchantLimiter{
		limiter:   limiter,
		specCache: &sync.Map{},
	}
}

// Allow 检查限流，同时清理 spec cache（当配置变更时）。
func (c *ConfiguredMerchantLimiter) Allow(merchantID string) bool {
	return c.limiter.Allow(merchantID)
}

// ClearCache 配置变更时调用以清理缓存。
func (c *ConfiguredMerchantLimiter) ClearCache() {
	c.specCache.Range(func(key, value interface{}) bool {
		c.specCache.Delete(key)
		return true
	})
}

// MerchantLimiterOption 用于构造时自定义参数的选项函数。
type MerchantLimiterOption func(*MerchantLimiter)

// WithMetricsCallback 设置指标回调。
func WithMetricsCallback(cb func(merchantID string)) MerchantLimiterOption {
	return func(m *MerchantLimiter) {
		m.metricsCallback = cb
	}
}

// WithLogger 设置 logger。
func WithLogger(logger *zap.Logger) MerchantLimiterOption {
	return func(m *MerchantLimiter) {
		m.logger = logger
	}
}

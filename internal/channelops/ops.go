// Package channelops 提供渠道运维能力：per-adapter 限流 + 渠道启停开关。
//
// 用法：payment-core 的 Charge 流程在 routing 之后、调 channel 之前：
//   1. channelops.IsEnabled(adapter) → false 直接返回 channel_disabled
//   2. channelops.Allow(adapter) → false 返回 rate_limited
//   3. 调 channel
//
// 管理面：admin-web 通过 BFF 调 payment-core 的 gRPC 管理接口更新配置。
// 配置存内存（进程重启恢复默认），prod 可以接配置中心 hot-push。
package channelops

import (
	"sync"

	"golang.org/x/time/rate"
)

// ChannelConfig 单个渠道的运维配置
type ChannelConfig struct {
	Enabled bool    // false = 禁用该渠道，直接拒绝
	RPS     float64 // 0 = 不限流
	Burst   int
}

// Manager 渠道运维管理器
type Manager struct {
	mu       sync.RWMutex
	configs  map[string]*ChannelConfig
	limiters map[string]*rate.Limiter
}

func NewManager() *Manager {
	return &Manager{
		configs:  make(map[string]*ChannelConfig),
		limiters: make(map[string]*rate.Limiter),
	}
}

// SetConfig 更新某渠道的运维配置（admin 调用）
func (m *Manager) SetConfig(adapter string, cfg ChannelConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[adapter] = &cfg
	if cfg.RPS > 0 {
		burst := cfg.Burst
		if burst <= 0 {
			burst = int(cfg.RPS)
			if burst < 1 {
				burst = 1
			}
		}
		m.limiters[adapter] = rate.NewLimiter(rate.Limit(cfg.RPS), burst)
	} else {
		delete(m.limiters, adapter)
	}
}

// IsEnabled 渠道是否启用。未配置的默认启用。
func (m *Manager) IsEnabled(adapter string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cfg, ok := m.configs[adapter]
	if !ok {
		return true // 未配置 = 默认启用
	}
	return cfg.Enabled
}

// Allow 渠道限流检查。未配置限流 = 放行。
func (m *Manager) Allow(adapter string) bool {
	m.mu.RLock()
	l, ok := m.limiters[adapter]
	m.mu.RUnlock()
	if !ok {
		return true
	}
	return l.Allow()
}

// GetAll 返回所有配置快照（admin 展示用）
func (m *Manager) GetAll() map[string]ChannelConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]ChannelConfig, len(m.configs))
	for k, v := range m.configs {
		out[k] = *v
	}
	return out
}

// Disable 快捷：禁用某渠道
func (m *Manager) Disable(adapter string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[adapter]
	if !ok {
		cfg = &ChannelConfig{Enabled: false}
		m.configs[adapter] = cfg
	} else {
		cfg.Enabled = false
	}
}

// Enable 快捷：启用某渠道
func (m *Manager) Enable(adapter string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[adapter]
	if !ok {
		cfg = &ChannelConfig{Enabled: true}
		m.configs[adapter] = cfg
	} else {
		cfg.Enabled = true
	}
}

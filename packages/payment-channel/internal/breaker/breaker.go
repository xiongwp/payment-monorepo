// Package breaker — path-level circuit breaker.
//
// 现在 payment-channel 每个 channel adapter 用 (channel_id) 粒度熔断, 太粗 —
// 一旦 channel 一个 path (e.g. /refund) 抽风, 整 channel 全熔断, 把好用的 /charge 也封.
//
// 这里实现 per-path 熔断:
//   key = channel_id + "|" + path (e.g. "stripe|/v1/refunds")
//
// 状态机 (跟 sony/gobreaker 一致):
//   Closed  → 正常请求
//   Open    → 拒绝请求 (5xx 直接返 ErrCircuitOpen)
//   HalfOpen → 允许一定流量探测; 成功 → Closed; 失败 → Open
//
// 触发条件 (可配):
//   - 5xx 错误率 > 50% 且最近 100 个请求 → Open
//   - Open 持续 30s → HalfOpen 允许 10 个 probe → 5 个成功 → Closed
//
// 不熔断的情况 (业务 4xx 错):
//   - 401 / 403 / 404 / 422 (商户参数 / 业务逻辑错, 不该熔断)
//   - 只看 5xx / network error / timeout / 429

package breaker

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// State
type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	}
	return "unknown"
}

var ErrCircuitOpen = errors.New("breaker: circuit open")

// Config 熔断配置
type Config struct {
	// 触发条件
	WindowSize       int           // 滑动窗口请求数, 默认 100
	FailureRate      float64       // 触发阈值, 默认 0.5 (50%)
	MinRequests      int           // 至少 N 个请求才评估, 默认 20

	// 状态机
	OpenDuration     time.Duration // Open 维持多久, 默认 30s
	HalfOpenMaxProbe int           // HalfOpen 允许多少 probe, 默认 10
	HalfOpenMinSucc  int           // probe 中至少多少成功才回 Closed, 默认 5
}

func DefaultConfig() Config {
	return Config{
		WindowSize:       100,
		FailureRate:      0.5,
		MinRequests:      20,
		OpenDuration:     30 * time.Second,
		HalfOpenMaxProbe: 10,
		HalfOpenMinSucc:  5,
	}
}

// Breaker 单 path 熔断器
type Breaker struct {
	key string
	cfg Config

	mu          sync.Mutex
	state       State
	openedAt    time.Time
	// 滑动窗口 (ring buffer)
	results     []bool // true=success, false=failure
	idx         int
	count       int // 已填满数

	// HalfOpen 期统计
	probeAtomic uint32 // 当前已发起 probe 数
	probeSucc   uint32 // 成功数
	probeFail   uint32 // 失败数
}

func newBreaker(key string, cfg Config) *Breaker {
	return &Breaker{
		key:     key,
		cfg:     cfg,
		state:   StateClosed,
		results: make([]bool, cfg.WindowSize),
	}
}

// State 返回当前状态 (lock-free 读, 可能 stale)
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.computeState()
}

// Allow 业务调用前检查; 返 nil 表示放行, 否则 ErrCircuitOpen.
func (b *Breaker) Allow() error {
	b.mu.Lock()
	s := b.computeState()
	b.mu.Unlock()
	switch s {
	case StateClosed:
		return nil
	case StateHalfOpen:
		// 限流到 HalfOpenMaxProbe 个 probe
		if atomic.AddUint32(&b.probeAtomic, 1) > uint32(b.cfg.HalfOpenMaxProbe) {
			atomic.AddUint32(&b.probeAtomic, ^uint32(0)) // -1
			return ErrCircuitOpen
		}
		return nil
	}
	return ErrCircuitOpen
}

// Record 业务调用结果. success = 2xx (不是 5xx / timeout / 网络错).
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case StateClosed:
		// 滚动窗口
		b.results[b.idx] = success
		b.idx = (b.idx + 1) % b.cfg.WindowSize
		if b.count < b.cfg.WindowSize {
			b.count++
		}
		// 评估
		if b.count >= b.cfg.MinRequests {
			fails := 0
			for i := 0; i < b.count; i++ {
				if !b.results[i] {
					fails++
				}
			}
			if float64(fails)/float64(b.count) >= b.cfg.FailureRate {
				b.openCircuit()
			}
		}
	case StateHalfOpen:
		if success {
			atomic.AddUint32(&b.probeSucc, 1)
		} else {
			atomic.AddUint32(&b.probeFail, 1)
		}
		succ := atomic.LoadUint32(&b.probeSucc)
		fail := atomic.LoadUint32(&b.probeFail)
		// 任一失败 → 立即回 Open (保守)
		if fail > 0 {
			b.openCircuit()
			return
		}
		// 集齐 HalfOpenMinSucc → 回 Closed
		if int(succ) >= b.cfg.HalfOpenMinSucc {
			b.closeCircuit()
		}
	}
}

// computeState — 必须持锁; 顺便处理 Open → HalfOpen 时间过渡.
func (b *Breaker) computeState() State {
	if b.state == StateOpen && time.Since(b.openedAt) >= b.cfg.OpenDuration {
		b.state = StateHalfOpen
		atomic.StoreUint32(&b.probeAtomic, 0)
		atomic.StoreUint32(&b.probeSucc, 0)
		atomic.StoreUint32(&b.probeFail, 0)
	}
	return b.state
}

func (b *Breaker) openCircuit() {
	b.state = StateOpen
	b.openedAt = time.Now()
}

func (b *Breaker) closeCircuit() {
	b.state = StateClosed
	for i := range b.results {
		b.results[i] = false
	}
	b.idx = 0
	b.count = 0
}

// ── Manager: 多 path 路由 ──

type Manager struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
	cfg      Config
}

func NewManager(cfg Config) *Manager {
	return &Manager{breakers: map[string]*Breaker{}, cfg: cfg}
}

// Get 获取或创建对应 (channel, path) 的 breaker.
func (m *Manager) Get(channel, path string) *Breaker {
	key := channel + "|" + path
	m.mu.RLock()
	b, ok := m.breakers[key]
	m.mu.RUnlock()
	if ok {
		return b
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.breakers[key]; ok {
		return b
	}
	b = newBreaker(key, m.cfg)
	m.breakers[key] = b
	return b
}

// Snapshot 给 admin / 监控用 — 列所有 breaker 当前状态.
type Stat struct {
	Key   string `json:"key"`
	State string `json:"state"`
}

func (m *Manager) Snapshot() []Stat {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Stat, 0, len(m.breakers))
	for k, b := range m.breakers {
		out = append(out, Stat{Key: k, State: b.State().String()})
	}
	return out
}

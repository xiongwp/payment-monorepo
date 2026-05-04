// Package circuitbreaker 实现简易熔断器。
//
// payment-core 在调 payment-channel 之前过一遍 breaker：
//   - Closed（正常）→ 放行
//   - Open（熔断）  → 直接返回 CHANNEL_UNAVAILABLE，不打下游
//   - HalfOpen      → 放有限流量做探测
//
// 触发条件：滑窗内连续失败 N 次 → Open。
// 恢复条件：Open 持续 T 秒后 → HalfOpen → 如果第一笔成功 → Closed。
//
// 粒度：按 adapter name 隔离（gcash breaker 不影响 maya）。
package circuitbreaker

import (
	"fmt"
	"sync"
	"time"

	"github.com/xiongwp/payment-core/internal/metrics"
)

type State int

const (
	Closed   State = 0
	Open     State = 1
	HalfOpen State = 2
)

func (s State) String() string {
	switch s {
	case Closed:
		return "CLOSED"
	case Open:
		return "OPEN"
	case HalfOpen:
		return "HALF_OPEN"
	}
	return "UNKNOWN"
}

// Config 熔断器参数
type Config struct {
	FailThreshold int           // 连续失败多少次后 Open（默认 5）
	OpenDuration  time.Duration // Open 状态持续多久后转 HalfOpen（默认 30s）
	HalfOpenMax   int           // HalfOpen 放多少笔探测（默认 1）
}

var DefaultConfig = Config{
	FailThreshold: 5,
	OpenDuration:  30 * time.Second,
	HalfOpenMax:   1,
}

// Breaker 单个 adapter 的熔断器实例
type Breaker struct {
	mu          sync.Mutex
	adapter     string // 用于 metric 标签
	cfg         Config
	state       State
	failures    int
	openSince   time.Time
	halfOpenReq int
}

func newBreaker(adapter string, cfg Config) *Breaker {
	if cfg.FailThreshold <= 0 {
		cfg.FailThreshold = DefaultConfig.FailThreshold
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = DefaultConfig.OpenDuration
	}
	if cfg.HalfOpenMax <= 0 {
		cfg.HalfOpenMax = DefaultConfig.HalfOpenMax
	}
	b := &Breaker{adapter: adapter, cfg: cfg, state: Closed}
	// 显式 set 一次 gauge：新 adapter 一上线就有可观测值（默认 0=Closed），
	// 不必等到首次状态变化才 export。
	metrics.CircuitState.WithLabelValues(adapter).Set(float64(Closed))
	return b
}

// transitionLocked 必须在已持有 b.mu 的前提下调用。
// 把 from→to 落到 metric 上；同状态自迁不计数（避免 Allow 中 Open→Open 路径重复打点）。
func (b *Breaker) transitionLocked(to State) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	if b.adapter == "" {
		return // 旧测试可能直接 new(Breaker)，无 adapter
	}
	metrics.CircuitTransitions.WithLabelValues(b.adapter, from.String(), to.String()).Inc()
	metrics.CircuitState.WithLabelValues(b.adapter).Set(float64(to))
}

// Allow 检查当前是否允许发请求。返回 false 表示熔断中。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case Open:
		if time.Since(b.openSince) >= b.cfg.OpenDuration {
			b.transitionLocked(HalfOpen)
			b.halfOpenReq = 1
			return true
		}
		return false
	case HalfOpen:
		if b.halfOpenReq < b.cfg.HalfOpenMax {
			b.halfOpenReq++
			return true
		}
		return false
	}
	return true
}

// RecordSuccess 标记一次成功
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	if b.state == HalfOpen {
		b.transitionLocked(Closed)
	}
}

// Reset forces the breaker back to Closed with a clean failure counter.
// Used by the /ops/circuit/reset admin endpoint so a stuck breaker doesn't
// require a service restart after a burst of QA/probe failures.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.halfOpenReq = 0
	b.openSince = time.Time{}
	b.transitionLocked(Closed)
}

// RecordFailure 标记一次失败
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.state == HalfOpen || b.failures >= b.cfg.FailThreshold {
		b.openSince = time.Now()
		b.failures = 0
		b.transitionLocked(Open)
	}
}

// State 当前状态（监控用）
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && time.Since(b.openSince) >= b.cfg.OpenDuration {
		return HalfOpen
	}
	return b.state
}

// ─── Registry：按 adapter name 管理多个 breaker ──────────────────

type Registry struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
	cfg      Config
}

func NewRegistry(cfg Config) *Registry {
	return &Registry{breakers: make(map[string]*Breaker), cfg: cfg}
}

func (r *Registry) Get(adapter string) *Breaker {
	r.mu.RLock()
	b, ok := r.breakers[adapter]
	r.mu.RUnlock()
	if ok {
		return b
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok = r.breakers[adapter]; ok {
		return b
	}
	b = newBreaker(adapter, r.cfg)
	r.breakers[adapter] = b
	return b
}

// Reset force-closes a single adapter's breaker. Returns false if the adapter
// has never issued a request (registry never created an entry). No-op on a
// never-opened breaker; admin UI should treat that as success anyway.
func (r *Registry) Reset(adapter string) bool {
	r.mu.RLock()
	b, ok := r.breakers[adapter]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	b.Reset()
	return true
}

// ResetAll force-closes every breaker. Unblocks QA after a batch of
// intentional failure scenarios.
func (r *Registry) ResetAll() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.breakers {
		b.Reset()
	}
	return len(r.breakers)
}

// States 返回所有 adapter 的当前状态（admin / metrics 用）
func (r *Registry) States() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.breakers))
	for k, b := range r.breakers {
		out[k] = b.State().String()
	}
	return out
}

// ErrCircuitOpen 熔断打开时的标准错误
var ErrCircuitOpen = fmt.Errorf("circuit breaker open: channel unavailable")

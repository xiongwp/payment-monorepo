// Package resilience 给 5 个 network adapter 提供熔断 + bulkhead，
// 保护 card-payment 不被某一家卡组织故障拖垮。
//
// 设计：
//   - 每个 network 一个 *Breaker（per-key isolation）
//   - 三态：Closed → Open → HalfOpen → (Closed | Open)
//   - 触发：rolling window 内连续 N 次失败 / 窗内 fail rate > 阈值
//   - 降级：Open 期间 Authorize 直接 fail-fast（不发 HTTPS），返
//     "circuit_open" decline，不污染统计窗
//   - 恢复：Open 持续 cooldown 后转 HalfOpen，放 1 笔探测；成功 → Closed，失败 → 回 Open
//
// 不重复造轮子：行为 / 阈值跟 payment-core/internal/circuitbreaker 对齐，
// 可观测性（state / transitions）也一致，保证全栈断路器语义统一。
package resilience

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// State 熔断器状态机
type State int32

const (
	StateClosed   State = 0
	StateOpen     State = 1
	StateHalfOpen State = 2
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateOpen:
		return "OPEN"
	case StateHalfOpen:
		return "HALF_OPEN"
	}
	return "UNKNOWN"
}

// ErrOpen 熔断器 Open 时直接返这个，caller 看见就 fail-fast 不发 HTTPS。
var ErrOpen = errors.New("resilience: circuit open")

// Config 单个 Breaker 的阈值。零值有 prod 友好的默认。
type Config struct {
	// 滑动窗口大小（按调用次数；满了才算判定一次）
	WindowSize int

	// 连续失败 ConsecutiveFailures 次直接 trip（fast-trip）
	ConsecutiveFailures int

	// 窗口内 fail-rate >= 这个比例 trip
	FailureRateThreshold float64

	// Open 状态持续时长，到点转 HalfOpen
	OpenDuration time.Duration

	// HalfOpen 探测一次失败 → 回 Open；成功 → Closed
	// 不需配置（默认行为）

	// 监控钩子：transitions 写到这里（对接 metrics.CircuitTransitions）
	OnTransition func(from, to State)
}

func (c *Config) defaults() {
	if c.WindowSize <= 0 {
		c.WindowSize = 50
	}
	if c.ConsecutiveFailures <= 0 {
		c.ConsecutiveFailures = 8
	}
	if c.FailureRateThreshold <= 0 {
		c.FailureRateThreshold = 0.5
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = 30 * time.Second
	}
}

// Breaker 单 key（如 "visa"）的断路器。
type Breaker struct {
	cfg    Config
	mu     sync.Mutex
	state  atomic.Int32 // State
	openAt time.Time

	// 滑窗：用 ring buffer 保留最近 WindowSize 笔结果
	window []bool // true = success；用 idx % len 写入
	idx    int
	full   bool

	consecutiveFails int
}

// New 构造。
func New(cfg Config) *Breaker {
	cfg.defaults()
	return &Breaker{
		cfg:    cfg,
		window: make([]bool, cfg.WindowSize),
	}
}

// State 当前状态（atomic，读不加锁）
func (b *Breaker) State() State { return State(b.state.Load()) }

// Allow 在调 network adapter 之前检查；返 false 时 caller 直接返 ErrOpen。
//
// HalfOpen 状态下：第一笔放过去探测；探测期间其它请求继续 Allow=false，
// 简化实现避开 thundering herd。
func (b *Breaker) Allow() bool {
	switch b.State() {
	case StateClosed:
		return true
	case StateOpen:
		// 检查是否到点转 HalfOpen
		b.mu.Lock()
		if time.Since(b.openAt) >= b.cfg.OpenDuration {
			b.transition(StateOpen, StateHalfOpen)
			b.mu.Unlock()
			return true // 这一笔作为探测放过去
		}
		b.mu.Unlock()
		return false
	case StateHalfOpen:
		// 已经有一笔在试；新请求挡住等结果
		return false
	}
	return false
}

// Record success / failure，更新内部窗口 + 状态机。caller 在 adapter 调
// 完后调一次（成功 / 失败都要记，否则窗口失真）。
func (b *Breaker) Record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cur := b.State()
	switch cur {
	case StateHalfOpen:
		if success {
			// 探测成功 → 复位
			b.window = make([]bool, b.cfg.WindowSize)
			b.idx = 0
			b.full = false
			b.consecutiveFails = 0
			b.transition(StateHalfOpen, StateClosed)
		} else {
			b.transition(StateHalfOpen, StateOpen)
			b.openAt = time.Now()
		}
		return
	case StateOpen:
		// 不应该发生（Allow 拒了），收到也不影响状态
		return
	}

	// StateClosed
	b.window[b.idx] = success
	b.idx = (b.idx + 1) % b.cfg.WindowSize
	if b.idx == 0 {
		b.full = true
	}
	if success {
		b.consecutiveFails = 0
		return
	}
	b.consecutiveFails++

	// fast-trip：连续失败次数到阈值
	if b.consecutiveFails >= b.cfg.ConsecutiveFailures {
		b.transition(StateClosed, StateOpen)
		b.openAt = time.Now()
		return
	}

	// rolling rate 触发：窗满后才计算
	if b.full {
		fails := 0
		for _, ok := range b.window {
			if !ok {
				fails++
			}
		}
		rate := float64(fails) / float64(len(b.window))
		if rate >= b.cfg.FailureRateThreshold {
			b.transition(StateClosed, StateOpen)
			b.openAt = time.Now()
		}
	}
}

// transition 必须在 b.mu 锁内调用。
func (b *Breaker) transition(from, to State) {
	if !b.state.CompareAndSwap(int32(from), int32(to)) {
		return // 已被别人改了
	}
	if b.cfg.OnTransition != nil {
		b.cfg.OnTransition(from, to)
	}
}

// ─── Registry：管 5 个 network 各一个 Breaker ──────────────────────────

// Registry per-network 字典 + 一致的 Reset / States 接口。
type Registry struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
}

// NewRegistry 给每个 network key 起一个独立 breaker。
//   keys: ["visa", "mastercard", "jcb", "amex", "unionpay"]
//   onTransition: 每次状态切换时回调（用来打 metrics）
func NewRegistry(keys []string, onTransition func(network string, from, to State)) *Registry {
	r := &Registry{breakers: make(map[string]*Breaker, len(keys))}
	for _, k := range keys {
		k := k
		var hook func(from, to State)
		if onTransition != nil {
			hook = func(from, to State) { onTransition(k, from, to) }
		}
		r.breakers[k] = New(Config{OnTransition: hook})
	}
	return r
}

// Get 取 network 对应 breaker；未注册返回 nil（caller 应当作 closed）
func (r *Registry) Get(network string) *Breaker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.breakers[network]
}

// States dump 所有 network 当前状态，给 ops admin endpoint 用。
func (r *Registry) States() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.breakers))
	for k, b := range r.breakers {
		out[k] = b.State().String()
	}
	return out
}

// Reset 强制把指定 network 拉回 Closed（ops 故障已修复后用）。
func (r *Registry) Reset(network string) bool {
	r.mu.RLock()
	b := r.breakers[network]
	r.mu.RUnlock()
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	from := b.State()
	if from == StateClosed {
		return false
	}
	b.window = make([]bool, b.cfg.WindowSize)
	b.idx = 0
	b.full = false
	b.consecutiveFails = 0
	b.transition(from, StateClosed)
	return true
}

// ResetAll 全部 reset；返 reset 个数。
func (r *Registry) ResetAll() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for k := range r.breakers {
		if r.Reset(k) {
			n++
		}
	}
	return n
}

// ─── 实现 processor.BreakerRegistry 接口 ───

// Allow network breaker 是否放行（未注册的 network 默认 true，向后兼容）
func (r *Registry) Allow(network string) bool {
	b := r.Get(network)
	if b == nil {
		return true
	}
	return b.Allow()
}

// Record adapter 调用结果上报。未注册的 network 直接忽略。
func (r *Registry) Record(network string, success bool) {
	b := r.Get(network)
	if b == nil {
		return
	}
	b.Record(success)
}

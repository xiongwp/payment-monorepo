// circuit.go — SP-AC-7 O3: 轻量 circuit breaker + 自带 metric 上报.
//
// 设计:
//   - 经典三态机: closed → open (failures 超阈值) → half_open (cooldown 后试探) → closed/open
//   - 无外部依赖 (sony/gobreaker 等), 内置 sync.Mutex + atomic counter
//   - 跟 metric 联动: state gauge + trips counter 自动维护
//
// 用法:
//   cb := observability.NewCircuitBreaker("accounting_grpc", observability.CircuitConfig{
//       FailureThreshold: 5,
//       SuccessThreshold: 2,
//       OpenDuration:     30 * time.Second,
//   })
//   if err := cb.Do(ctx, func() error { return client.Call() }); err != nil {
//       // 可能是 ErrCircuitOpen (fail-fast) 或下游真错
//   }
//
// 半开状态行为:
//   - open → cooldown 过期 → 自动半开
//   - 半开期允许 1 个 in-flight 探针; 探针成功 N 次 → close, 失败 1 次 → 再 open
package observability

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrCircuitOpen 由 Do 返回, 表示当前断路器是 open, 调用没真发出去.
var ErrCircuitOpen = errors.New("circuit breaker open")

// CircuitState — 0=closed, 1=half_open, 2=open. 跟 metric label 一致.
type CircuitState int32

const (
	CircuitClosed   CircuitState = 0
	CircuitHalfOpen CircuitState = 1
	CircuitOpen     CircuitState = 2
)

// CircuitConfig.
type CircuitConfig struct {
	FailureThreshold int           // 连续失败到此数 → trip 到 open. 默认 5.
	SuccessThreshold int           // half_open 连续成功此数 → close. 默认 2.
	OpenDuration     time.Duration // open 状态持续多久后自动转 half_open. 默认 30s.
}

// CircuitBreaker.
type CircuitBreaker struct {
	name string
	cfg  CircuitConfig

	mu              sync.Mutex
	state           atomic.Int32 // CircuitState
	failures        int
	halfOpenSuccess int
	openAt          time.Time
}

// NewCircuitBreaker.
func NewCircuitBreaker(name string, cfg CircuitConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 2
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 30 * time.Second
	}
	cb := &CircuitBreaker{name: name, cfg: cfg}
	cb.state.Store(int32(CircuitClosed))
	CircuitStateGauge.WithLabelValues(name).Set(0)
	return cb
}

// Do 在断路器保护下跑 fn. open → 立即返 ErrCircuitOpen.
func (cb *CircuitBreaker) Do(_ context.Context, fn func() error) error {
	if !cb.beforeCall() {
		return ErrCircuitOpen
	}
	err := fn()
	cb.afterCall(err)
	return err
}

// beforeCall 检查是否允许调用. 返 false 表示 open, 调用方不该真调.
func (cb *CircuitBreaker) beforeCall() bool {
	state := CircuitState(cb.state.Load())
	switch state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		// 看 cooldown 是否过期
		cb.mu.Lock()
		defer cb.mu.Unlock()
		if time.Since(cb.openAt) >= cb.cfg.OpenDuration {
			cb.state.Store(int32(CircuitHalfOpen))
			cb.halfOpenSuccess = 0
			CircuitStateGauge.WithLabelValues(cb.name).Set(1)
			return true // 让这个调用作为探针
		}
		return false
	case CircuitHalfOpen:
		// half-open 期允许探针 (严格的实现限制并发探针数, 这里简化为允许所有, 反正失败会立刻 trip)
		return true
	}
	return true
}

// afterCall 记录 fn 调用结果, 推进状态机.
func (cb *CircuitBreaker) afterCall(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state := CircuitState(cb.state.Load())
	if err == nil {
		// 成功
		cb.failures = 0
		switch state {
		case CircuitHalfOpen:
			cb.halfOpenSuccess++
			if cb.halfOpenSuccess >= cb.cfg.SuccessThreshold {
				// 探针够数 → 恢复 closed
				cb.state.Store(int32(CircuitClosed))
				cb.halfOpenSuccess = 0
				CircuitStateGauge.WithLabelValues(cb.name).Set(0)
			}
		}
		return
	}
	// 失败
	cb.failures++
	switch state {
	case CircuitClosed:
		if cb.failures >= cb.cfg.FailureThreshold {
			cb.state.Store(int32(CircuitOpen))
			cb.openAt = time.Now()
			CircuitStateGauge.WithLabelValues(cb.name).Set(2)
			CircuitTrips.WithLabelValues(cb.name).Inc()
		}
	case CircuitHalfOpen:
		// 半开期失败 → 立刻 trip 回 open
		cb.state.Store(int32(CircuitOpen))
		cb.openAt = time.Now()
		cb.halfOpenSuccess = 0
		CircuitStateGauge.WithLabelValues(cb.name).Set(2)
		CircuitTrips.WithLabelValues(cb.name).Inc()
	}
}

// CurrentState 返当前状态 (testing / log 用).
func (cb *CircuitBreaker) CurrentState() CircuitState {
	return CircuitState(cb.state.Load())
}

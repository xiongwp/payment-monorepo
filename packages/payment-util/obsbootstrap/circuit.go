// circuit.go — 复用 split-payment 的轻量 circuit breaker, 抽到共享.
//
// 用法:
//	cb := obsbootstrap.NewCircuit("accounting_http", obsbootstrap.CircuitConfig{
//	    FailureThreshold: 5, OpenDuration: 30*time.Second,
//	})
//	if err := cb.Do(ctx, func() error { return client.Call() }); err != nil { ... }
//
// metric: <service>_circuit_state{downstream=name} (0=closed/1=halfopen/2=open) +
//          <service>_circuit_trips_total. 当前用全局 namespace "obs", 服务量多时 dashboard
//          按 downstream label 区分.
package obsbootstrap

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	circuitState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "circuit",
		Name:      "state",
		Help:      "Circuit breaker state per downstream (0=closed, 1=half_open, 2=open).",
	}, []string{"downstream"})

	circuitTrips = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "obs",
		Subsystem: "circuit",
		Name:      "trips_total",
		Help:      "Total circuit breaker trips.",
	}, []string{"downstream"})
)

// ErrCircuitOpen 调用被熔断器 fail-fast 拦下时返回.
var ErrCircuitOpen = errors.New("circuit breaker open")

type cstate int32

const (
	cClosed   cstate = 0
	cHalfOpen cstate = 1
	cOpen     cstate = 2
)

// CircuitConfig.
type CircuitConfig struct {
	FailureThreshold int           // 连续失败到此数 → trip 到 open. 默认 5.
	SuccessThreshold int           // half_open 连续成功此数 → close. 默认 2.
	OpenDuration     time.Duration // open 持续多久 → 转 half_open. 默认 30s.
}

// CircuitBreaker.
type CircuitBreaker struct {
	name string
	cfg  CircuitConfig

	mu              sync.Mutex
	state           atomic.Int32
	failures        int
	halfOpenSuccess int
	openAt          time.Time
}

// NewCircuit 构造.
func NewCircuit(name string, cfg CircuitConfig) *CircuitBreaker {
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
	cb.state.Store(int32(cClosed))
	circuitState.WithLabelValues(name).Set(0)
	return cb
}

// Do 在断路器保护下执行 fn. open → ErrCircuitOpen, fn 不会被调.
func (cb *CircuitBreaker) Do(_ context.Context, fn func() error) error {
	if !cb.beforeCall() {
		return ErrCircuitOpen
	}
	err := fn()
	cb.afterCall(err)
	return err
}

func (cb *CircuitBreaker) beforeCall() bool {
	state := cstate(cb.state.Load())
	switch state {
	case cClosed:
		return true
	case cOpen:
		cb.mu.Lock()
		defer cb.mu.Unlock()
		if time.Since(cb.openAt) >= cb.cfg.OpenDuration {
			cb.state.Store(int32(cHalfOpen))
			cb.halfOpenSuccess = 0
			circuitState.WithLabelValues(cb.name).Set(1)
			return true
		}
		return false
	case cHalfOpen:
		return true
	}
	return true
}

func (cb *CircuitBreaker) afterCall(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	state := cstate(cb.state.Load())
	if err == nil {
		cb.failures = 0
		if state == cHalfOpen {
			cb.halfOpenSuccess++
			if cb.halfOpenSuccess >= cb.cfg.SuccessThreshold {
				cb.state.Store(int32(cClosed))
				cb.halfOpenSuccess = 0
				circuitState.WithLabelValues(cb.name).Set(0)
			}
		}
		return
	}
	cb.failures++
	switch state {
	case cClosed:
		if cb.failures >= cb.cfg.FailureThreshold {
			cb.state.Store(int32(cOpen))
			cb.openAt = time.Now()
			circuitState.WithLabelValues(cb.name).Set(2)
			circuitTrips.WithLabelValues(cb.name).Inc()
		}
	case cHalfOpen:
		cb.state.Store(int32(cOpen))
		cb.openAt = time.Now()
		cb.halfOpenSuccess = 0
		circuitState.WithLabelValues(cb.name).Set(2)
		circuitTrips.WithLabelValues(cb.name).Inc()
	}
}

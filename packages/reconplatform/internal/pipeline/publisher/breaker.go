// breaker.go — 极简 circuit breaker, 给 KafkaPublisher 用 (REL-1).
//
// 状态机:
//
//	Closed     ── N 连续失败 ──>  Open
//	Open       ── cooldown 到 ──>  HalfOpen
//	HalfOpen   ── 一次成功     ──>  Closed
//	HalfOpen   ── 一次失败     ──>  Open  (重置 cooldown)
//
// 用于 Kafka publisher:
//   - Closed:   照常 Produce.
//   - Open:     fail-fast,publisher 跳过 Kafka 直接走 DLQ (避免 produce 队列堆爆).
//   - HalfOpen: 放一条试探, 看 Kafka 恢复了没.
//
// 不上 sony/gobreaker — 只这一处用,~ 50 行实现避免引入依赖.
package publisher

import (
	"sync"
	"sync/atomic"
	"time"
)

// breakerState 内部状态.
type breakerState int32

const (
	bsClosed breakerState = iota
	bsOpen
	bsHalfOpen
)

// circuitBreaker 简易 CB. 线程安全.
type circuitBreaker struct {
	mu                sync.Mutex
	state             breakerState
	failureCount      atomic.Int64
	failureThreshold  int64
	openedAt          time.Time
	cooldown          time.Duration
	transitions       atomic.Int64 // metrics

	// OnTransition 状态变化回调 (可选), 便于打 metric / log.
	OnTransition func(from, to string)
}

// newCircuitBreaker 默认 5 连续失败开,30s 后试探.
func newCircuitBreaker(threshold int64, cooldown time.Duration) *circuitBreaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &circuitBreaker{
		state:            bsClosed,
		failureThreshold: threshold,
		cooldown:         cooldown,
	}
}

// Allow 返 true 表示可以发请求; false 则 fail-fast.
//
// 在 Open 状态下 cooldown 到了会 trip 一次到 HalfOpen 并允许一次试探;
// 试探期间多并发请求只允许第一个进去 (再次返 false 给后续).
func (b *circuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case bsClosed:
		return true
	case bsHalfOpen:
		// HalfOpen 只放一个,再来都 reject.
		return false
	case bsOpen:
		if time.Since(b.openedAt) > b.cooldown {
			b.transitionLocked(bsHalfOpen)
			return true
		}
		return false
	}
	return false
}

// OnSuccess 成功一条.
func (b *circuitBreaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case bsClosed:
		b.failureCount.Store(0)
	case bsHalfOpen:
		b.transitionLocked(bsClosed)
		b.failureCount.Store(0)
	}
}

// OnFailure 失败一条.
func (b *circuitBreaker) OnFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case bsClosed:
		n := b.failureCount.Add(1)
		if n >= b.failureThreshold {
			b.transitionLocked(bsOpen)
		}
	case bsHalfOpen:
		// 试探失败立刻回 Open
		b.transitionLocked(bsOpen)
	case bsOpen:
		// 没事,继续等 cooldown
	}
}

// State 当前状态字符串 (admin / metric 用).
func (b *circuitBreaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case bsClosed:
		return "closed"
	case bsOpen:
		return "open"
	case bsHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// transitionLocked 必须在 mu 持锁下调.
func (b *circuitBreaker) transitionLocked(to breakerState) {
	if b.state == to {
		return
	}
	from := b.state
	b.state = to
	if to == bsOpen {
		b.openedAt = time.Now()
	}
	b.transitions.Add(1)
	if b.OnTransition != nil {
		go b.OnTransition(stateName(from), stateName(to))
	}
}

func stateName(s breakerState) string {
	switch s {
	case bsClosed:
		return "closed"
	case bsOpen:
		return "open"
	case bsHalfOpen:
		return "half_open"
	}
	return "unknown"
}

// Package reliability 风控主路径下游依赖的可靠性保护：
//   - Breaker 熔断器：连续失败 → 短路；超时不再打下游
//   - 限流：per-merchant Screen QPS（计费 + 防滥用）
//
// 每个外部依赖（ipintel / mlscore）单独一个 Breaker 实例，互不影响。
// 主路径 fail-open：Breaker Open 时直接返回零值（IPIntel.Result{}, MLResult{Score:0}），
// Screen 主流程不阻塞。
package reliability

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// State 熔断器状态。
type State int32

const (
	// Closed 正常通过；失败累计达 fail_threshold → Open。
	Closed State = 0
	// Open 直接短路；持续 open_duration 后允许 HalfOpen 探测。
	Open State = 1
	// HalfOpen 允许 N 笔（默认 1）探测；成功 → Closed，失败 → Open 重新计时。
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

// Config 熔断器参数。零值字段会替换成下面的默认。
type Config struct {
	Name          string
	FailThreshold int           // 连续失败多少次 → Open；默认 5
	OpenDuration  time.Duration // Open 持续多久 → HalfOpen；默认 15s
	HalfOpenMax   int32         // HalfOpen 允许并发探测笔数；默认 1
}

func defaults(c Config) Config {
	if c.FailThreshold <= 0 {
		c.FailThreshold = 5
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = 15 * time.Second
	}
	if c.HalfOpenMax <= 0 {
		c.HalfOpenMax = 1
	}
	return c
}

// ErrCircuitOpen 熔断打开时调用方拿到的哨兵错误（caller 应识别并 fail-open）。
var ErrCircuitOpen = errors.New("reliability: circuit open")

// Breaker 单依赖熔断器。线程安全。
type Breaker struct {
	cfg          Config
	mu           sync.Mutex
	state        State
	consecFail   int
	openedAt     time.Time
	halfOpenInFlight int32
}

func NewBreaker(c Config) *Breaker {
	return &Breaker{cfg: defaults(c), state: Closed}
}

// Allow 判断当前是否允许打下游。返回 false 表示该 fail-open / 跳过下游调用。
// HalfOpen 模式下最多允许 cfg.HalfOpenMax 个并发探测；超出也返 false。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if time.Since(b.openedAt) >= b.cfg.OpenDuration {
			b.state = HalfOpen
			b.halfOpenInFlight = 0
		} else {
			return false
		}
		fallthrough
	case HalfOpen:
		if atomic.LoadInt32(&b.halfOpenInFlight) >= b.cfg.HalfOpenMax {
			return false
		}
		atomic.AddInt32(&b.halfOpenInFlight, 1)
		return true
	}
	return true
}

// OnSuccess 调用方下游成功后报。HalfOpen → Closed；Closed 重置 fail 计数。
func (b *Breaker) OnSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case HalfOpen:
		b.state = Closed
		b.consecFail = 0
		atomic.StoreInt32(&b.halfOpenInFlight, 0)
	case Closed:
		b.consecFail = 0
	}
}

// OnFailure 调用方下游失败 / 超时后报。
// Closed: 累计连续失败；达阈值 → Open
// HalfOpen: 探测失败 → Open 重新计时
func (b *Breaker) OnFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		b.consecFail++
		if b.consecFail >= b.cfg.FailThreshold {
			b.state = Open
			b.openedAt = time.Now()
		}
	case HalfOpen:
		b.state = Open
		b.openedAt = time.Now()
		atomic.StoreInt32(&b.halfOpenInFlight, 0)
	}
}

// State 返回当前状态（admin / metrics 用）。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

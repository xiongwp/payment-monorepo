// Adaptive concurrency limiter — Netflix Gradient2 style。
//
// 动机：现有 MerchantLimiter 是静态 token bucket（按预设 RPS 拒）。它无法
// 识别"下游慢"的早期信号——当 ML / IPIntel 等下游 RTT 拉长，inflight 请求堆
// 在 Screen 主路径里，goroutine 与 heap 双爆，最终 OOM。token bucket 在
// 这种情况下毫无感知（QPS 没超阈值，rps 数字漂亮）。
//
// AdaptiveLimiter 盯的是 *RTT 变化*：
//   - 长窗（默认 60s 半衰期）EMA 平均；短窗（默认 5s 半衰期）EMA 平均
//   - gradient = rtt_long / rtt_short
//     · gradient < 1 → 当前在拉慢下游 → 缩 inflight cap
//     · gradient ≥ 1 → 下游还有空间 → 放 cap
//   - newLimit = (limit * gradient + queueSize)·α + limit·(1-α)
//     再叠加探索扰动 → clamp [min, max]
//   - 单调下降锁死保护：limit ≤ min 后 inflight 空闲一段时间 → 强制回到 min 的 1.5×
//
// 与 MerchantLimiter 互补：
//   - MerchantLimiter 看 RPS，按套餐计费 + 防滥用
//   - AdaptiveLimiter 看 inflight & RTT，保护服务不雪崩
//
// 关键不变量：
//   - Acquire 计数原子；release 必然把 inflight 减回，调用方 defer release()
//   - 默认 enable=false（构造期 limit/min/max 仍然有值；admin 显式开启才生效）
//   - per-merchant 状态走 LRU（默认 1000）；evict 时丢弃 → 下次 Acquire 视为冷启动
//
// 算法参数（默认匹配 Gradient2 paper 经验值）：
//   - smoothingAlpha = 0.5（newLimit 权重一半新值一半旧值，避免抖动）
//   - exploration    = 0.2（20% 概率往上探一格，破单调下降锁死）
//   - queueSize      = sqrt(limit)（Little's law 经验：M/M/1 队列）
package reliability

import (
	"errors"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// ErrLimitExceeded inflight 超 cap 时 Acquire 返回的哨兵错误。
// caller 应识别并返 HTTP 503 / gRPC ResourceExhausted（**不要 hang**）。
var ErrLimitExceeded = errors.New("reliability: adaptive limit exceeded")

// AdaptiveConfig 构造参数。零值字段在 NewAdaptive 内替换成下面注释里的默认。
type AdaptiveConfig struct {
	// Initial 初始 limit（启动期样本不够时兜底）。默认 20
	Initial int
	// Min limit 下限（防过度收缩到 0）。默认 5
	Min int
	// Max limit 上限（防探索冲过单实例承载量）。默认 200
	Max int
	// LongHalfLife 长窗 EMA 半衰期；默认 60s
	LongHalfLife time.Duration
	// ShortHalfLife 短窗 EMA 半衰期；默认 5s
	ShortHalfLife time.Duration
	// SmoothingAlpha newLimit = limit*gradient*(1-alpha) + alpha*limit；默认 0.5
	SmoothingAlpha float64
	// ExplorationRate 每次调整有 P 概率往上探一格（破锁死）。默认 0.2
	ExplorationRate float64
	// MerchantCap LRU 上限（防 cardinality 爆）。默认 1000
	MerchantCap int
	// GlobalCap 所有 merchant 加起来的 inflight 上限（兜底，> 0 才启用）。默认 0=off
	GlobalCap int
	// Enabled 默认 false → Acquire 永远放行，只采样 RTT 不拒绝。
	// admin 显式 SetEnabled(true) 才真正限流。
	Enabled bool
}

func adaptiveDefaults(c AdaptiveConfig) AdaptiveConfig {
	if c.Initial <= 0 {
		c.Initial = 20
	}
	if c.Min <= 0 {
		c.Min = 5
	}
	if c.Max <= 0 {
		c.Max = 200
	}
	if c.Min > c.Max {
		c.Min = c.Max
	}
	if c.Initial < c.Min {
		c.Initial = c.Min
	}
	if c.Initial > c.Max {
		c.Initial = c.Max
	}
	if c.LongHalfLife <= 0 {
		c.LongHalfLife = 60 * time.Second
	}
	if c.ShortHalfLife <= 0 {
		c.ShortHalfLife = 5 * time.Second
	}
	if c.SmoothingAlpha <= 0 || c.SmoothingAlpha >= 1 {
		c.SmoothingAlpha = 0.5
	}
	if c.ExplorationRate < 0 || c.ExplorationRate >= 1 {
		c.ExplorationRate = 0.2
	}
	if c.MerchantCap <= 0 {
		c.MerchantCap = 1000
	}
	return c
}

// limiterState per-merchant 状态。所有可变字段在 state.mu 保护下；inflight
// 走 atomic（高频路径，免锁）。
type limiterState struct {
	mu sync.Mutex
	// 当前 limit（float64 让 gradient 乘除更平滑；Acquire 比较时取 int）。
	limit float64
	// 短/长窗 EMA RTT（秒）。
	rttShort float64
	rttLong  float64
	// 上次更新 EMA 的时间戳，给 time-decay 公式用。
	lastUpdate time.Time
	// 上次 limit 调整时间（限制 limit 更新频率，不让每次 release 都重算）。
	lastAdjust time.Time
	// 上次 inflight > 0 的时间。用来判定"空闲太久"触发锁死保护。
	lastBusy time.Time
	// 累计拒绝数（admin 看）。
	rejected uint64
	// inflight：atomic int64，Acquire/Release 不进 mu。
	inflight int64
	// 当前快照 limit（int，对外读）—— 保持 float limit 的 round。
	limitInt int64
}

// AdaptiveLimiter 主类型。
//
// 实现要点：
//   - merchants map + LRU 列表共享 mu（写）；读路径用 RWMutex 给 mostly-read。
//   - 全局 inflight 走 atomic int64（GlobalCap > 0 才检查）。
//   - 算法核心 observe() 在每次 release 时被调，但内部按 minAdjustInterval（200ms）
//     节流，避免高频 release 导致 limit 抖动。
type AdaptiveLimiter struct {
	cfg AdaptiveConfig

	// enabled 单独原子位（admin 频繁读改），不锁整张表。
	enabled atomic.Bool

	mu         sync.RWMutex
	merchants  map[string]*lruNode
	lruHead    *lruNode // 最近用
	lruTail    *lruNode // 最旧
	merchantsN int

	globalInflight atomic.Int64

	// 全局拒绝计数（无 merchant 维度时用，比如 globalCap 触发）。
	globalRejected atomic.Uint64

	// RNG（探索扰动），protected by rngMu。
	rngMu sync.Mutex
	rng   *rand.Rand

	// onLimitChange optional callback（metric / log）。nil-safe。
	onLimitChange func(merchantID string, oldLimit, newLimit int)
	onReject      func(merchantID string, reason string)
	onRTT         func(merchantID string, rttShort, rttLong float64)

	// minAdjustNanos limit 调整最短间隔（纳秒；atomic，避免 observe 内
	// 加 l.mu.RLock 与持 st.mu 的潜在 lock-ordering 问题）。
	minAdjustNanos atomic.Int64
}

// lruNode merchants map 的双向链表节点。同时持 limiterState 引用，让 Acquire
// 路径不需要再 map lookup（拿到 node.state 就够）。
type lruNode struct {
	merchantID string
	state      *limiterState
	prev, next *lruNode
}

// NewAdaptive 构造一个 AdaptiveLimiter；cfg 零值字段走 adaptiveDefaults。
// 默认 disabled —— 调用方应在 main.go 通过 admin endpoint / 启动时 SetEnabled(true)
// 显式打开。
func NewAdaptive(cfg AdaptiveConfig) *AdaptiveLimiter {
	cfg = adaptiveDefaults(cfg)
	l := &AdaptiveLimiter{
		cfg:               cfg,
		merchants:         make(map[string]*lruNode),
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	l.minAdjustNanos.Store(int64(200 * time.Millisecond))
	l.enabled.Store(cfg.Enabled)
	return l
}

// SetEnabled 在线开关（admin endpoint 调）。disabled 时 Acquire 永远放行，
// 但仍采样 RTT —— 让运营能先看 metric 评估再 enable。
func (l *AdaptiveLimiter) SetEnabled(v bool) { l.enabled.Store(v) }

// Enabled 当前状态。
func (l *AdaptiveLimiter) Enabled() bool { return l.enabled.Load() }

// SetCallbacks 注入 metric / log 回调。nil 允许，单独传也允许。
func (l *AdaptiveLimiter) SetCallbacks(
	onLimitChange func(merchantID string, oldLimit, newLimit int),
	onReject func(merchantID string, reason string),
	onRTT func(merchantID string, rttShort, rttLong float64),
) {
	l.mu.Lock()
	l.onLimitChange = onLimitChange
	l.onReject = onReject
	l.onRTT = onRTT
	l.mu.Unlock()
}

// SetMinAdjustInterval 调整 limit 重算最短间隔（默认 200ms）。测试用，
// 让单测能在毫秒内跑出多次调整。
func (l *AdaptiveLimiter) SetMinAdjustInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	l.minAdjustNanos.Store(int64(d))
}

// Acquire 试占一个 slot。merchantID 空时走 "_unknown" 共享桶。
// 返回的 release 必然 non-nil（即使 err 也返一个 no-op release 让
// defer release() 永远安全）。
//
// disabled 状态：返回的 release 仍会记录 RTT（喂 EMA）但不会拒绝。
func (l *AdaptiveLimiter) Acquire(merchantID string) (release func(), err error) {
	if merchantID == "" {
		merchantID = "_unknown"
	}
	st := l.stateFor(merchantID)
	start := time.Now()

	enabled := l.enabled.Load()
	// 全局兜底（如果配了）：先抢全局 slot，再抢 per-merchant。
	if enabled && l.cfg.GlobalCap > 0 {
		gNow := l.globalInflight.Add(1)
		if int(gNow) > l.cfg.GlobalCap {
			l.globalInflight.Add(-1)
			l.globalRejected.Add(1)
			atomic.AddUint64(&st.rejected, 1)
			if cb := l.onRejectCb(); cb != nil {
				cb(merchantID, "global")
			}
			return noopRelease, ErrLimitExceeded
		}
	}

	// per-merchant 检查：先原子加 inflight，再比 limit；超了原子减回。
	// 这样 fast path 全程不进 state.mu。
	curLimit := atomic.LoadInt64(&st.limitInt)
	now := atomic.AddInt64(&st.inflight, 1)
	if enabled && now > curLimit {
		atomic.AddInt64(&st.inflight, -1)
		if l.cfg.GlobalCap > 0 {
			l.globalInflight.Add(-1)
		}
		atomic.AddUint64(&st.rejected, 1)
		if cb := l.onRejectCb(); cb != nil {
			cb(merchantID, "limit")
		}
		return noopRelease, ErrLimitExceeded
	}

	// release：记 RTT + 减 inflight。capture st by closure。
	var released atomic.Bool
	return func() {
		if !released.CompareAndSwap(false, true) {
			return
		}
		rtt := time.Since(start).Seconds()
		atomic.AddInt64(&st.inflight, -1)
		if l.cfg.GlobalCap > 0 {
			l.globalInflight.Add(-1)
		}
		l.observe(merchantID, st, rtt)
	}, nil
}

// observe 喂 RTT 到 EMA、按节流间隔触发 limit 重算。
func (l *AdaptiveLimiter) observe(merchantID string, st *limiterState, rtt float64) {
	if rtt < 0 {
		rtt = 0
	}
	st.mu.Lock()
	now := time.Now()
	// 初始化第一个样本：直接置 EMA = 当前样本。
	if st.lastUpdate.IsZero() {
		st.rttShort = rtt
		st.rttLong = rtt
		st.lastUpdate = now
		st.lastAdjust = now
		st.lastBusy = now
		st.mu.Unlock()
		return
	}
	// 时间衰减 EMA：alpha = 1 - exp(-dt * ln2 / halfLife)
	dt := now.Sub(st.lastUpdate).Seconds()
	if dt < 0 {
		dt = 0
	}
	aShort := emaAlpha(dt, l.cfg.ShortHalfLife.Seconds())
	aLong := emaAlpha(dt, l.cfg.LongHalfLife.Seconds())
	st.rttShort = aShort*rtt + (1-aShort)*st.rttShort
	st.rttLong = aLong*rtt + (1-aLong)*st.rttLong
	st.lastUpdate = now
	if atomic.LoadInt64(&st.inflight) > 0 {
		st.lastBusy = now
	}
	// 节流：limit 调整间隔（atomic 读，避免 observe 持 st.mu 时再抢 l.mu）
	minAdj := time.Duration(l.minAdjustNanos.Load())
	adjust := now.Sub(st.lastAdjust) >= minAdj
	if !adjust {
		rs, rl := st.rttShort, st.rttLong
		st.mu.Unlock()
		if cb := l.onRTTCb(); cb != nil {
			cb(merchantID, rs, rl)
		}
		return
	}
	st.lastAdjust = now

	oldLimit := st.limit
	newLimit := l.computeNewLimit(st, now)
	st.limit = newLimit
	st.limitInt = int64(math.Round(newLimit))
	rs, rl := st.rttShort, st.rttLong
	st.mu.Unlock()

	if cb := l.onLimitChangeCb(); cb != nil && int(math.Round(oldLimit)) != int(math.Round(newLimit)) {
		cb(merchantID, int(math.Round(oldLimit)), int(math.Round(newLimit)))
	}
	if cb := l.onRTTCb(); cb != nil {
		cb(merchantID, rs, rl)
	}
}

// computeNewLimit Gradient2 核心。调用方持 st.mu。
//
// gradient = rtt_long / rtt_short
//   - rtt_short 突然变大（下游慢）→ gradient < 1 → 缩
//   - rtt_short 比 rtt_long 还快 → gradient > 1 → 放
//
// queueSize ≈ sqrt(limit)（Little's law）作为目标允许的小队列长度。
// 探索扰动：以 P 概率把 newLimit + 1，避免单调下降到 min 后困在 min。
// 锁死保护：limit ≤ min 且 inflight 长时间为 0（≥ 5s）→ 强行加到 min * 1.5
// 重新探。
func (l *AdaptiveLimiter) computeNewLimit(st *limiterState, now time.Time) float64 {
	limit := st.limit
	if limit <= 0 {
		limit = float64(l.cfg.Initial)
	}
	rs := st.rttShort
	rl := st.rttLong
	var gradient float64
	switch {
	case rs <= 0 || rl <= 0:
		gradient = 1 // 样本不够 → 保持
	default:
		gradient = rl / rs
	}
	// clamp gradient 到 [0.5, 1.5] —— Gradient2 paper：单步调整不超过 ±50%
	// 否则 RTT 抖一下 limit 直接砍半。
	if gradient < 0.5 {
		gradient = 0.5
	}
	if gradient > 1.5 {
		gradient = 1.5
	}
	queue := math.Sqrt(limit)
	target := limit*gradient + queue
	// 平滑：α 比例新值
	alpha := l.cfg.SmoothingAlpha
	next := alpha*target + (1-alpha)*limit

	// 锁死保护：limit 已经在 min 附近 + 一段时间没活跃 → 抬一抬。
	if limit <= float64(l.cfg.Min)+0.5 && now.Sub(st.lastBusy) > 5*time.Second {
		next = float64(l.cfg.Min) * 1.5
	}

	// 探索扰动：随机 +1（不下探 —— 下探由 gradient 自然做）
	l.rngMu.Lock()
	exp := l.rng.Float64() < l.cfg.ExplorationRate
	l.rngMu.Unlock()
	if exp {
		next += 1
	}

	if next < float64(l.cfg.Min) {
		next = float64(l.cfg.Min)
	}
	if next > float64(l.cfg.Max) {
		next = float64(l.cfg.Max)
	}
	return next
}

// emaAlpha 时间衰减 EMA 系数。dt=halfLife → alpha=0.5。
func emaAlpha(dt, halfLife float64) float64 {
	if halfLife <= 0 || dt <= 0 {
		return 0
	}
	a := 1 - math.Exp(-dt*math.Ln2/halfLife)
	if a < 0 {
		return 0
	}
	if a > 1 {
		return 1
	}
	return a
}

// stateFor 拿/造 per-merchant state；同时更新 LRU 位置。
// 写路径（新建 / move-to-head）走 mu.Lock，其余 RLock 即可。
func (l *AdaptiveLimiter) stateFor(merchantID string) *limiterState {
	l.mu.RLock()
	if n, ok := l.merchants[merchantID]; ok {
		st := n.state
		isHead := n == l.lruHead
		l.mu.RUnlock()
		if !isHead {
			l.mu.Lock()
			// double-check：可能已被 evict
			if cn, ok2 := l.merchants[merchantID]; ok2 && cn == n {
				l.moveToHeadLocked(n)
			}
			l.mu.Unlock()
		}
		return st
	}
	l.mu.RUnlock()

	// miss：新建。
	l.mu.Lock()
	defer l.mu.Unlock()
	if n, ok := l.merchants[merchantID]; ok {
		l.moveToHeadLocked(n)
		return n.state
	}
	st := &limiterState{
		limit:    float64(l.cfg.Initial),
		limitInt: int64(l.cfg.Initial),
	}
	n := &lruNode{merchantID: merchantID, state: st}
	l.merchants[merchantID] = n
	l.pushHeadLocked(n)
	l.merchantsN++
	// LRU evict
	for l.merchantsN > l.cfg.MerchantCap && l.lruTail != nil {
		old := l.lruTail
		l.removeLocked(old)
		delete(l.merchants, old.merchantID)
		l.merchantsN--
	}
	return st
}

// pushHeadLocked / moveToHeadLocked / removeLocked 简单双向链表操作。l.mu Locked。
func (l *AdaptiveLimiter) pushHeadLocked(n *lruNode) {
	n.prev = nil
	n.next = l.lruHead
	if l.lruHead != nil {
		l.lruHead.prev = n
	}
	l.lruHead = n
	if l.lruTail == nil {
		l.lruTail = n
	}
}

func (l *AdaptiveLimiter) moveToHeadLocked(n *lruNode) {
	if n == l.lruHead {
		return
	}
	l.removeLocked(n)
	l.pushHeadLocked(n)
}

func (l *AdaptiveLimiter) removeLocked(n *lruNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else if l.lruHead == n {
		l.lruHead = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else if l.lruTail == n {
		l.lruTail = n.prev
	}
	n.prev, n.next = nil, nil
}

// --- snapshot for admin ---

// MerchantSnapshot 单 merchant 的当前快照。admin GET 用。
type MerchantSnapshot struct {
	MerchantID string  `json:"merchant_id"`
	Limit      int     `json:"limit"`
	Inflight   int64   `json:"inflight"`
	Rejected   uint64  `json:"rejected"`
	RTTShort   float64 `json:"rtt_short_seconds"`
	RTTLong    float64 `json:"rtt_long_seconds"`
}

// Snapshot 全量快照（按当前 LRU 顺序，最新使用在前）。
func (l *AdaptiveLimiter) Snapshot() []MerchantSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]MerchantSnapshot, 0, l.merchantsN)
	for n := l.lruHead; n != nil; n = n.next {
		st := n.state
		st.mu.Lock()
		out = append(out, MerchantSnapshot{
			MerchantID: n.merchantID,
			Limit:      int(st.limitInt),
			Inflight:   atomic.LoadInt64(&st.inflight),
			Rejected:   atomic.LoadUint64(&st.rejected),
			RTTShort:   st.rttShort,
			RTTLong:    st.rttLong,
		})
		st.mu.Unlock()
	}
	return out
}

// GlobalSnapshot 全局聚合（global cap / 全局 reject 计数）。
func (l *AdaptiveLimiter) GlobalSnapshot() (inflight int64, rejected uint64, enabled bool, globalCap int) {
	return l.globalInflight.Load(), l.globalRejected.Load(), l.enabled.Load(), l.cfg.GlobalCap
}

// SetLimit 强制设某 merchant 的 limit（debug / admin override）。
// clamp 到 [Min, Max]。
func (l *AdaptiveLimiter) SetLimit(merchantID string, limit int) {
	if merchantID == "" {
		merchantID = "_unknown"
	}
	st := l.stateFor(merchantID)
	if limit < l.cfg.Min {
		limit = l.cfg.Min
	}
	if limit > l.cfg.Max {
		limit = l.cfg.Max
	}
	st.mu.Lock()
	st.limit = float64(limit)
	st.limitInt = int64(limit)
	st.mu.Unlock()
}

// Reset 把所有 merchant 的 limit 重置到 Initial；RTT EMA / rejected 计数也清零。
// admin 用，比如想跳出某个锁死状态。
func (l *AdaptiveLimiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for n := l.lruHead; n != nil; n = n.next {
		st := n.state
		st.mu.Lock()
		st.limit = float64(l.cfg.Initial)
		st.limitInt = int64(l.cfg.Initial)
		st.rttShort = 0
		st.rttLong = 0
		st.lastUpdate = time.Time{}
		st.lastAdjust = time.Time{}
		st.lastBusy = time.Time{}
		atomic.StoreUint64(&st.rejected, 0)
		st.mu.Unlock()
	}
	l.globalRejected.Store(0)
}

// Config 返回当前配置（不可变副本）。
func (l *AdaptiveLimiter) Config() AdaptiveConfig { return l.cfg }

// callback 读：sync.RWMutex 读路径。
func (l *AdaptiveLimiter) onLimitChangeCb() func(string, int, int) {
	l.mu.RLock()
	cb := l.onLimitChange
	l.mu.RUnlock()
	return cb
}
func (l *AdaptiveLimiter) onRejectCb() func(string, string) {
	l.mu.RLock()
	cb := l.onReject
	l.mu.RUnlock()
	return cb
}
func (l *AdaptiveLimiter) onRTTCb() func(string, float64, float64) {
	l.mu.RLock()
	cb := l.onRTT
	l.mu.RUnlock()
	return cb
}

// noopRelease defer release() 永远安全：err 路径返这个，调用方不必判 err。
func noopRelease() {}

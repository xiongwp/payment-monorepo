// reservation.go: 限额预扣记录簿（store 包内，rules / service 都可用，避免循环引用）。
//
// 资损修复 P1-1 配套：规则评估时往 AtomicCounter 写预扣，service 层在最终
// 决策前用本结构跟踪当前请求所有预扣项；最终决策为 Deny / Review 时调
// CancelAll() 把所有预扣回滚（防止"请求被拒，但限额已被锁住直到 TTL 才释放"
// 这种"软DDoS"问题）。
//
// 同时给 IdempotencyKey 兜底：Screen 时已预扣过的 key，Report 阶段不再
// 重复 Incr（避免双重计算）。
//
// 注意：本 tracker 是**请求级**对象（per Screen call），不持久化跨进程；
// 进程在预扣后崩溃时，依赖 Counter 自身 TTL 让预扣自然失效。这是"重启丢失
// 状态"的兜底：预扣不变成永久锁。

package store

import (
	"context"
	"sync"
)

// ReservationKind 预扣维度。
type ReservationKind int

const (
	ReservationDaily ReservationKind = iota
	ReservationMonthly
	ReservationVelocity       // 笔数
	ReservationVelocityAmount // 金额
)

// Reservation 单条预扣条目。
type Reservation struct {
	Kind      ReservationKind
	Key       string
	Amount    int64
	WindowMin int
}

// ReservationTracker 单次 Screen 内的所有预扣项（线程安全）。
type ReservationTracker struct {
	mu    sync.Mutex
	items []Reservation
}

func NewReservationTracker() *ReservationTracker {
	return &ReservationTracker{}
}

func (t *ReservationTracker) Add(r Reservation) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.items = append(t.items, r)
}

func (t *ReservationTracker) Items() []Reservation {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Reservation, len(t.items))
	copy(out, t.items)
	return out
}

// CancelAll 调底层 AtomicCounter 把所有预扣回滚。Counter 不实现 AtomicCounter
// 时 no-op（理论上规则也不会预扣，所以无需 cancel）。
func (t *ReservationTracker) CancelAll(ctx context.Context, counter Counter) {
	if t == nil || counter == nil {
		return
	}
	ac, ok := counter.(AtomicCounter)
	if !ok {
		return
	}
	t.mu.Lock()
	items := make([]Reservation, len(t.items))
	copy(items, t.items)
	t.items = nil
	t.mu.Unlock()
	for _, r := range items {
		switch r.Kind {
		case ReservationDaily:
			ac.CancelDaily(ctx, r.Key, r.Amount)
		case ReservationMonthly:
			ac.CancelMonthly(ctx, r.Key, r.Amount)
		case ReservationVelocity:
			ac.CancelVelocity(ctx, r.Key, r.WindowMin)
		case ReservationVelocityAmount:
			ac.CancelVelocityAmount(ctx, r.Key, r.Amount, r.WindowMin)
		}
	}
}

func (t *ReservationTracker) Empty() bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items) == 0
}

type reservationCtxKey struct{}

// WithReservationTracker 把 tracker 注入 context。service.Screen 入口调一次。
func WithReservationTracker(ctx context.Context, tr *ReservationTracker) context.Context {
	if tr == nil {
		return ctx
	}
	return context.WithValue(ctx, reservationCtxKey{}, tr)
}

// ReservationFromContext 从 context 取 tracker；规则 Evaluate 内调，
// nil 表示当前调用方未启用预扣机制 → 规则退回非原子的 Get + 比较 行为。
func ReservationFromContext(ctx context.Context) *ReservationTracker {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(reservationCtxKey{}).(*ReservationTracker)
	return v
}

// atomic_counter.go: Counter 接口的"原子限额预扣"扩展能力。
//
// 目的（资损修复 P1-1）：原 Counter API 只暴露 GetDaily / GetMonthly /
// GetVelocity / GetVelocityAmount + Incr。规则评估流是 "Get → 比较 → 决策"
// 然后 Report 时再 Incr → 高并发下两次 Get 之间发生其它 goroutine 的 Incr，
// TOCTOU race 让"拆单绕过限额"成为可能：
//
//	T1 GetDaily=9000  (limit=10000)            T2 GetDaily=9000
//	T1 9000+800=9800 < 10000 → ALLOW           T2 9000+800=9800 < 10000 → ALLOW
//	... Report 后 T1 Incr=9800                 ... Report 后 T2 Incr=10600  ← 超限
//
// 修复：把 "比较 + 累计" 做成原子操作。AtomicCounter 接口提供：
//
//	IncrIfBelow(key, delta, limit, ttl) (newVal, ok, err)
//
// 内部用 Lua 脚本（Redis）或 Mutex（Mem）保证：要么 INCRBY 后 newVal <= limit
// 返 ok=true；否则 DECRBY 回滚返 ok=false（"预扣失败 → 限额命中"）。
//
// 使用模型：reserve/commit/cancel
//   1. Screen 阶段：amount_limit / velocity / velocity_amount 调
//      IncrIfBelow 预扣；ok=false → 限额命中，返 Hit
//   2. 决策汇总：所有规则跑完，Decision == Allow → 保留预扣（commit）；
//      Decision == Deny / Review → 撤销预扣（cancel = DECRBY 回滚）
//   3. Report 阶段：Screen 时已预扣过同 IdempotencyKey 的不再 Incr，
//      避免双重计算
//
// Counter 实现是否支持原子语义由 AtomicCounter 接口断言决定。规则代码
// 在能拿到 AtomicCounter 时走原子路径；拿不到（旧 fake / 第三方实现）
// 则退回原 Get + Incr 行为（保持向后兼容）。

package store

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// AtomicCounter 在 Counter 之上扩展原子限额预扣能力。MemCounter 和
// RedisCounter 都实现它；规则代码用 type assertion 探测。
type AtomicCounter interface {
	IncrIfBelowDaily(ctx context.Context, key string, delta, limit int64, ttl time.Duration) (newVal int64, ok bool, err error)
	IncrIfBelowMonthly(ctx context.Context, key string, delta, limit int64, ttl time.Duration) (newVal int64, ok bool, err error)
	IncrIfBelowVelocity(ctx context.Context, key string, windowMin, maxCount int) (newCount int, ok bool, err error)
	IncrIfBelowVelocityAmount(ctx context.Context, key string, amount, maxAmount int64, windowMin int) (newSum int64, ok bool, err error)
	CancelDaily(ctx context.Context, key string, delta int64)
	CancelMonthly(ctx context.Context, key string, delta int64)
	CancelVelocity(ctx context.Context, key string, windowMin int)
	CancelVelocityAmount(ctx context.Context, key string, amount int64, windowMin int)
}

// memAtomicMu 给 MemCounter 的原子操作加一把互斥锁。复合操作 "比较 + 累加"
// 必须串行化，否则两个 goroutine 都拿到 9000 → 都判 9800 通过 → 都 Add(800)
// → 累计 9800 但实际为 10600。粒度：全局一把（Mem 给单测/dev 用，锁竞争
// 不是问题；生产走 Redis Lua）。
var memAtomicMu sync.Mutex

func (c *MemCounter) IncrIfBelowDaily(_ context.Context, key string, delta, limit int64, _ time.Duration) (int64, bool, error) {
	if delta <= 0 || limit <= 0 || key == "" {
		return 0, true, nil
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	dk := time.Now().UTC().Format("20060102") + ":" + key
	dval, _ := c.daily.LoadOrStore(dk, new(atomic.Int64))
	a := dval.(*atomic.Int64)
	cur := a.Load()
	if cur+delta > limit {
		return cur, false, nil
	}
	a.Add(delta)
	return cur + delta, true, nil
}

func (c *MemCounter) IncrIfBelowMonthly(_ context.Context, key string, delta, limit int64, _ time.Duration) (int64, bool, error) {
	if delta <= 0 || limit <= 0 || key == "" {
		return 0, true, nil
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	mk := time.Now().UTC().Format("200601") + ":" + key
	mval, _ := c.monthly.LoadOrStore(mk, new(atomic.Int64))
	a := mval.(*atomic.Int64)
	cur := a.Load()
	if cur+delta > limit {
		return cur, false, nil
	}
	a.Add(delta)
	return cur + delta, true, nil
}

func (c *MemCounter) IncrIfBelowVelocity(_ context.Context, key string, windowMin, maxCount int) (int, bool, error) {
	if maxCount <= 0 || key == "" {
		return 0, true, nil
	}
	if windowMin <= 0 {
		windowMin = 1
	}
	if windowMin > velocityBuckets {
		windowMin = velocityBuckets
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	vv, _ := c.velocity.LoadOrStore(key, newVelocityWindow())
	w := vv.(*velocityWindow)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.expireOldBuckets(now)
	curMinute := now.Unix() / 60
	count := 0
	for i := 0; i < windowMin; i++ {
		minute := curMinute - int64(i)
		idx := int(minute % int64(velocityBuckets))
		if idx < 0 {
			idx += velocityBuckets
		}
		if w.bucketStart[idx] == minute {
			count += w.bucketCount[idx]
		}
	}
	if count+1 > maxCount {
		return count, false, nil
	}
	idx := int(curMinute % int64(velocityBuckets))
	if idx < 0 {
		idx += velocityBuckets
	}
	if w.bucketStart[idx] != curMinute {
		w.bucketStart[idx] = curMinute
		w.bucketAmount[idx] = 0
		w.bucketCount[idx] = 0
	}
	w.bucketCount[idx]++
	return count + 1, true, nil
}

func (c *MemCounter) IncrIfBelowVelocityAmount(_ context.Context, key string, amount, maxAmount int64, windowMin int) (int64, bool, error) {
	if amount <= 0 || maxAmount <= 0 || key == "" {
		return 0, true, nil
	}
	if windowMin <= 0 {
		windowMin = 1
	}
	if windowMin > velocityBuckets {
		windowMin = velocityBuckets
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	vv, _ := c.velocity.LoadOrStore(key, newVelocityWindow())
	w := vv.(*velocityWindow)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	w.expireOldBuckets(now)
	curMinute := now.Unix() / 60
	var sum int64
	for i := 0; i < windowMin; i++ {
		minute := curMinute - int64(i)
		idx := int(minute % int64(velocityBuckets))
		if idx < 0 {
			idx += velocityBuckets
		}
		if w.bucketStart[idx] == minute {
			sum += w.bucketAmount[idx]
		}
	}
	if sum+amount > maxAmount {
		return sum, false, nil
	}
	idx := int(curMinute % int64(velocityBuckets))
	if idx < 0 {
		idx += velocityBuckets
	}
	if w.bucketStart[idx] != curMinute {
		w.bucketStart[idx] = curMinute
		w.bucketAmount[idx] = 0
		w.bucketCount[idx] = 0
	}
	w.bucketAmount[idx] += amount
	return sum + amount, true, nil
}

func (c *MemCounter) CancelDaily(_ context.Context, key string, delta int64) {
	if delta <= 0 || key == "" {
		return
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	dk := time.Now().UTC().Format("20060102") + ":" + key
	if v, ok := c.daily.Load(dk); ok {
		a := v.(*atomic.Int64)
		cur := a.Load()
		newV := cur - delta
		if newV < 0 {
			newV = 0
		}
		a.Store(newV)
	}
}

func (c *MemCounter) CancelMonthly(_ context.Context, key string, delta int64) {
	if delta <= 0 || key == "" {
		return
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	mk := time.Now().UTC().Format("200601") + ":" + key
	if v, ok := c.monthly.Load(mk); ok {
		a := v.(*atomic.Int64)
		cur := a.Load()
		newV := cur - delta
		if newV < 0 {
			newV = 0
		}
		a.Store(newV)
	}
}

func (c *MemCounter) CancelVelocity(_ context.Context, key string, _ int) {
	if key == "" {
		return
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	vv, ok := c.velocity.Load(key)
	if !ok {
		return
	}
	w := vv.(*velocityWindow)
	w.mu.Lock()
	defer w.mu.Unlock()
	curMinute := time.Now().Unix() / 60
	idx := int(curMinute % int64(velocityBuckets))
	if idx < 0 {
		idx += velocityBuckets
	}
	if w.bucketStart[idx] == curMinute && w.bucketCount[idx] > 0 {
		w.bucketCount[idx]--
	}
}

func (c *MemCounter) CancelVelocityAmount(_ context.Context, key string, amount int64, _ int) {
	if amount <= 0 || key == "" {
		return
	}
	memAtomicMu.Lock()
	defer memAtomicMu.Unlock()
	vv, ok := c.velocity.Load(key)
	if !ok {
		return
	}
	w := vv.(*velocityWindow)
	w.mu.Lock()
	defer w.mu.Unlock()
	curMinute := time.Now().Unix() / 60
	idx := int(curMinute % int64(velocityBuckets))
	if idx < 0 {
		idx += velocityBuckets
	}
	if w.bucketStart[idx] == curMinute {
		w.bucketAmount[idx] -= amount
		if w.bucketAmount[idx] < 0 {
			w.bucketAmount[idx] = 0
		}
	}
}

// 类型断言：MemCounter 实现 AtomicCounter 接口（编译期保证）。
var _ AtomicCounter = (*MemCounter)(nil)

// Package ratelimit: per-key token bucket。一个 key（IP / merchant_id）一只
// rate.Limiter，惰性创建。固定大小 ring 做最近活跃 key 的近似 LRU 淘汰，防止
// map 在长尾扫描攻击下无限膨胀。
//
// 不在 hot path 上锁全局 mutex —— 用 sync.RWMutex 让 Allow 走读锁；只在新
// key 第一次出现时升级写锁。
package ratelimit

import (
	"sync"

	"golang.org/x/time/rate"
)

// 默认最大保留 key 数。超过后随机淘汰一个（map iteration order 已经接近随机）。
// 实际部署可在 NewKeyed 之上扩展自定义淘汰策略；此处保持简单。
const defaultMaxKeys = 100_000

// Keyed per-key token bucket 容器。零值不可用，必须 NewKeyed 构造。
type Keyed struct {
	rps    rate.Limit
	burst  int
	maxKey int

	mu       sync.RWMutex
	limiters map[string]*rate.Limiter
}

// NewKeyed 构造一个 Keyed 限流器。rps <= 0 → Allow 始终返回 true（不限流）。
// burst <= 0 时退到 rps（取整），且至少 1。
func NewKeyed(rps rate.Limit, burst int) *Keyed {
	if burst <= 0 {
		if rps > 0 {
			burst = int(rps)
		}
		if burst < 1 {
			burst = 1
		}
	}
	return &Keyed{
		rps:      rps,
		burst:    burst,
		maxKey:   defaultMaxKeys,
		limiters: make(map[string]*rate.Limiter),
	}
}

// Allow 对 key 取 1 token；返回是否允许通过。rps <= 0 → 直接返回 true。
func (k *Keyed) Allow(key string) bool {
	if k.rps <= 0 {
		return true
	}
	k.mu.RLock()
	lim, ok := k.limiters[key]
	k.mu.RUnlock()
	if ok {
		return lim.Allow()
	}
	// 首次出现 key：升级写锁创建。double-check 避免并发重复创建。
	k.mu.Lock()
	if lim, ok = k.limiters[key]; !ok {
		// 容量保护：超过 maxKey 时随机淘汰一个。Go map iteration 顺序随机，
		// 拿一个 range 出的 key 删掉即可——不一定 LRU 但成本 O(1)，长期看
		// 等价于随机淘汰。
		if len(k.limiters) >= k.maxKey {
			for ek := range k.limiters {
				delete(k.limiters, ek)
				break
			}
		}
		lim = rate.NewLimiter(k.rps, k.burst)
		k.limiters[key] = lim
	}
	k.mu.Unlock()
	return lim.Allow()
}

// Len 返回当前活跃 key 数。监控用。
func (k *Keyed) Len() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.limiters)
}

// SetLimit 在线热更新 rps + burst（config-center 推送后调）。
//
// 实现：先更新 k.rps/k.burst（影响后续新 key），再 walk 全部已存在的子 limiter
// 调 SetLimit / SetBurst。锁内 O(N) 扫一次；N 通常 < 10K（活跃 IP / merchant 数）。
//
// 调用方不保证频繁调（admin 改一次配置才扇出一次），扫开销可接受。
func (k *Keyed) SetLimit(rps rate.Limit, burst int) {
	if burst <= 0 {
		if rps > 0 {
			burst = int(rps)
		}
		if burst < 1 {
			burst = 1
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rps = rps
	k.burst = burst
	for _, lim := range k.limiters {
		lim.SetLimit(rps)
		lim.SetBurst(burst)
	}
}

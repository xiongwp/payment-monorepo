// bulkhead.go：单租户隔离。
//
// 场景：商户 A 的客户出现支付死循环（前端 bug 或 attack），瞬间发出 1000 笔
// Authorize。如果 card-payment 直接全放过去：
//   - 卡组织 5xx 触发熔断（影响商户 B/C）
//   - DB pool 被 A 吃光（B/C 写不进 audit_log）
//   - HTTPS 出口 socket 被 A 占满（B/C dial 排队）
//
// Bulkhead = 单 merchant_id 同时在飞最多 N 笔。超出直接 fail-fast，A 自己背
// 锅，B/C 不受影响。
//
// 实现：每 merchant 一个 buffered channel（cap=N），Acquire push 一个 token，
// Release pop。channel 满 → Acquire return false fail-fast。
//
// 不做：
//   - 跨进程共享（每实例独立计数；K8s 多 pod 时总并发 = N × replicas，可接受）
//   - 优先级队列（首版）
package resilience

import (
	"sync"
	"sync/atomic"
)

// Bulkhead per-merchant 并发隔离。
type Bulkhead struct {
	defaultMax int

	mu   sync.RWMutex
	pool map[string]chan struct{} // merchant_id → token bucket

	// 监控：当前活跃 + 累计拒绝
	active   atomic.Int64
	rejected atomic.Int64
}

// NewBulkhead 构造。defaultMax = 单 merchant 并发上限。
// 0 / 负数走 32 兜底（保护 prod 配置漏填）。
func NewBulkhead(defaultMax int) *Bulkhead {
	if defaultMax <= 0 {
		defaultMax = 32
	}
	return &Bulkhead{
		defaultMax: defaultMax,
		pool:       make(map[string]chan struct{}),
	}
}

// OnReject 拒绝时的 hook（caller 把它指到 metrics counter）。
// 跟 Registry.OnTransition 一样的设计，避免 resilience 包反向依赖 metrics。
var OnRejectHook func()

// Acquire 拿一个 token；返 false 时 caller fail-fast。merchant_id 空走全局
// 单 bucket（key=""），仍然受 defaultMax 限制（防止匿名流量打爆）。
//
// 调用方必须在成功后用 defer h.Release(merchantID) 配对释放。
func (h *Bulkhead) Acquire(merchantID string) bool {
	bucket := h.bucketFor(merchantID)
	select {
	case bucket <- struct{}{}:
		h.active.Add(1)
		return true
	default:
		h.rejected.Add(1)
		if OnRejectHook != nil {
			OnRejectHook()
		}
		return false
	}
}

// Release pop 一个 token。Acquire 返 false 时**不要**调 Release（mismatch）。
func (h *Bulkhead) Release(merchantID string) {
	bucket := h.bucketFor(merchantID)
	select {
	case <-bucket:
		h.active.Add(-1)
	default:
		// 不应该发生（acquire/release 不配对）；忽略避免 panic
	}
}

// Stats 给 ops admin endpoint / metrics 用。
func (h *Bulkhead) Stats() (active, rejected int64, perMerchant int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.active.Load(), h.rejected.Load(), h.defaultMax
}

// bucketFor 取或建 merchant 的 channel。读多写少 → RLock fast path。
func (h *Bulkhead) bucketFor(merchantID string) chan struct{} {
	h.mu.RLock()
	if b, ok := h.pool[merchantID]; ok {
		h.mu.RUnlock()
		return b
	}
	h.mu.RUnlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	// 双检防 race
	if b, ok := h.pool[merchantID]; ok {
		return b
	}
	b := make(chan struct{}, h.defaultMax)
	h.pool[merchantID] = b
	return b
}

package channel

import "sync"

// DefaultPaymentChannelRegistry 基于 sync.RWMutex 的默认实现
type DefaultPaymentChannelRegistry struct {
	mu sync.RWMutex
	m  map[string]PaymentChannel
}

// NewDefaultPaymentChannelRegistry 构造
func NewDefaultPaymentChannelRegistry() *DefaultPaymentChannelRegistry {
	return &DefaultPaymentChannelRegistry{m: map[string]PaymentChannel{}}
}

// Register 注册 / 覆盖 payment_method → PaymentChannel
func (r *DefaultPaymentChannelRegistry) Register(paymentMethod string, ch PaymentChannel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[paymentMethod] = ch
}

// Get 查找；未注册返回 nil
func (r *DefaultPaymentChannelRegistry) Get(paymentMethod string) PaymentChannel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[paymentMethod]
}

// All 只读快照
func (r *DefaultPaymentChannelRegistry) All() map[string]PaymentChannel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]PaymentChannel, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

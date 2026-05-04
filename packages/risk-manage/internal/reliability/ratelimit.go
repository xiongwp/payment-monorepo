package reliability

import (
	"sync"
	"time"
)

// MerchantLimiter per-merchant Screen QPS 限流，token bucket 实现。
//
// 用途：
//   - 防滥用：单商户突发 10k QPS 不会拖死全平台
//   - 计费：套餐里写明 Starter=50 QPS, Pro=500 QPS；超额返回 ResourceExhausted
//   - DoS 防护：异常商户密钥泄漏 / 客户端 bug 不会无限刷
//
// 每商户独立 bucket；bucket 用懒补充（lazy refill）：调用 Allow 时按 elapsed
// 时间补 token，不需要后台 ticker。
//
// 高并发下：bucket 内部 mutex，单商户多并发会有锁竞争。20k QPS 可接受
// （每个 Allow 微秒级），更高换 atomic float64 + CAS。
type MerchantLimiter struct {
	mu       sync.RWMutex
	buckets  map[string]*tokenBucket
	defaults LimitConfig
	custom   map[string]LimitConfig // merchant_id → 商户专属配额（套餐升级用）
}

type LimitConfig struct {
	RPS   float64 // 每秒令牌数（= sustained QPS 上限）
	Burst float64 // bucket 容量（= 突发上限）
}

func (c LimitConfig) zero() bool { return c.RPS <= 0 && c.Burst <= 0 }

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	rps      float64
	capacity float64
}

func newBucket(cfg LimitConfig) *tokenBucket {
	return &tokenBucket{
		tokens:   cfg.Burst,
		last:     time.Now(),
		rps:      cfg.RPS,
		capacity: cfg.Burst,
	}
}

// NewMerchantLimiter 默认配额给所有商户共享；custom 给套餐升级 / 大客户单独
// 配（"merchant_id":{rps,burst}）。defaults.RPS<=0 → 限流完全 disabled
// （所有 Allow 直接 true）。
func NewMerchantLimiter(defaults LimitConfig, custom map[string]LimitConfig) *MerchantLimiter {
	return &MerchantLimiter{
		buckets:  make(map[string]*tokenBucket),
		defaults: defaults,
		custom:   custom,
	}
}

// Allow 试图扣 1 个 token。空 merchant_id 永远放行（内部调用 / 健康检查）。
// disabled defaults → 永远放行。
func (l *MerchantLimiter) Allow(merchantID string) bool {
	if merchantID == "" || l.defaults.zero() {
		return true
	}
	b := l.bucketFor(merchantID)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.rps
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *MerchantLimiter) bucketFor(merchantID string) *tokenBucket {
	l.mu.RLock()
	if b, ok := l.buckets[merchantID]; ok {
		l.mu.RUnlock()
		return b
	}
	l.mu.RUnlock()

	cfg := l.defaults
	if c, ok := l.custom[merchantID]; ok && !c.zero() {
		cfg = c
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[merchantID]; ok {
		return b
	}
	b := newBucket(cfg)
	l.buckets[merchantID] = b
	return b
}

// SetCustom 给商户套餐升级 / 降级（admin 端点写入）。
func (l *MerchantLimiter) SetCustom(merchantID string, cfg LimitConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.custom == nil {
		l.custom = make(map[string]LimitConfig)
	}
	l.custom[merchantID] = cfg
	// 当前 bucket 留着；新 cfg 在 bucket 拿不到 token 后下次新建生效。
	// 急切替换的话需 delete(l.buckets, merchantID)；这里宽松处理避免限流震荡。
}

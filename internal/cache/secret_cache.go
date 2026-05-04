package cache

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// SecretCacheHook 命中/未命中埋点；nil 允许。
type SecretCacheHook interface {
	Lookup(hit bool)
}

// SecretCache 存 (merchant_id, channel) → field_name → plaintext 的解密结果。
// BulkGetPlaintext 是 payment-channel adapter-init 的热路径；miss 一次要调 N 次
// KMS Decrypt，打进 RTT 数十毫秒。短 TTL（默认 5 分钟）兜住安全性 + 再 rotate 时
// 的收敛延迟。
//
// 单独给 (m,c) 一个条目是因为我们按 (m,c) 粒度整包取 —— 单字段粒度反而更复杂
// （需要按 field 做部分覆写）。
type SecretCache struct {
	entries *expirable.LRU[string, map[string]string]
	hook    SecretCacheHook
}

const (
	defaultSecretSize = 1024
	defaultSecretTTL  = 5 * time.Minute
)

// NewSecretCache 构造。size/ttl<=0 取默认（1024 / 5m）。
func NewSecretCache(size int, ttl time.Duration, hook SecretCacheHook) *SecretCache {
	if size <= 0 {
		size = defaultSecretSize
	}
	if ttl <= 0 {
		ttl = defaultSecretTTL
	}
	return &SecretCache{
		entries: expirable.NewLRU[string, map[string]string](size, nil, ttl),
		hook:    hook,
	}
}

func secretCacheKey(merchantID, channel string) string {
	return merchantID + "|" + channel
}

// Get 返回 (field→plaintext) 的副本；miss 返回 nil, false。
// 返回副本是为了让调用方不小心改了返回 map 也不会污染缓存条目。
func (c *SecretCache) Get(merchantID, channel string) (map[string]string, bool) {
	v, ok := c.entries.Get(secretCacheKey(merchantID, channel))
	if c.hook != nil {
		c.hook.Lookup(ok)
	}
	if !ok {
		return nil, false
	}
	copied := make(map[string]string, len(v))
	for k, val := range v {
		copied[k] = val
	}
	return copied, true
}

// Put 塞入；调用方传进来的 map 会被原样持有，外部不应再改动。
func (c *SecretCache) Put(merchantID, channel string, fields map[string]string) {
	if merchantID == "" || channel == "" || fields == nil {
		return
	}
	c.entries.Add(secretCacheKey(merchantID, channel), fields)
}

// Invalidate 删 (merchant, channel) 一个桶 —— Put/Delete 后调用。
func (c *SecretCache) Invalidate(merchantID, channel string) {
	c.entries.Remove(secretCacheKey(merchantID, channel))
}

// InvalidateMerchant 删某商户所有渠道的桶。当前简单实现：遍历 Keys 删匹配的。
// 量级小（单租户 < 10 条），可接受。
func (c *SecretCache) InvalidateMerchant(merchantID string) {
	prefix := merchantID + "|"
	for _, k := range c.entries.Keys() {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			c.entries.Remove(k)
		}
	}
}

// Purge 清空。
func (c *SecretCache) Purge() { c.entries.Purge() }

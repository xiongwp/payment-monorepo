package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// IntrospectCacheHook 命中/未命中埋点；nil 允许。
// 与 SecretCacheHook 不复用是为了让 user-merchant-core 的指标按维度分开。
type IntrospectCacheHook interface {
	Lookup(hit bool)
}

// IntrospectCacheValue 缓存的 introspect 结果。语义上=service.IntrospectResult，
// 但放在 cache 包不依赖 service 包，避免反向依赖。
//
// 注意：缓存只装 Valid==true 的结果。Valid==false 的结果不缓存——
//
//   - 让恶意 token 仍被每次 DB 校验，避免变成 DOS amplifier；
//   - 用户 logout 后失效路径需要在 100% 流量上看到，而不是 30s 后才生效。
type IntrospectCacheValue struct {
	UserID        int64
	EmailVerified bool
	ExpiresAt     time.Time
	Scopes        []string
	Permissions   []string
	// CachedAt 写入缓存的时刻，给 staleness 调试 + LogoutAll 时按 user_id 清理用。
	CachedAt time.Time
}

// IntrospectCache 进程内 JWT introspect 缓存。
//
// 设计取舍：
//
//	(1) Key 用 SHA256(jwt) 而不是 jwt 原文：JWT 是 Bearer 凭证，
//	    放在进程内存里是攻击面；hash 后即使 dump heap 也拿不到原 token。
//	    冲突概率：sha256 在 10⁹ 量级 birthday boundary 才有意义，远超 cap。
//	(2) TTL 默认 30s：登出生效延迟 ≤ 30s。生产可调；不要 > 5min（PCI 合规）。
//	(3) 不缓存 Valid==false：避免无效 token DOS amplifier，
//	    且让 logout / risk-freeze 能在 100% 流量上即时生效。
//	(4) 单条 cap 默认 100k：内存 ~10MB（permissions 列表大约 100 byte / item）。
//	    实际生产按 ActiveSessions × 1.5 配。
type IntrospectCache struct {
	entries *expirable.LRU[string, IntrospectCacheValue]
	// userIndex: user_id -> 这个 user 在缓存里的所有 hashedKey 集合。
	// LogoutAll 用这个 O(k) 删除该用户所有 token 而不用扫全表（cap 可能 100k+）。
	// expirable 的 evict 回调会同步删 userIndex 对应项，避免悬挂引用。
	userIndex *expirable.LRU[int64, map[string]struct{}]
	hook      IntrospectCacheHook
	ttl       time.Duration
}

const (
	defaultIntrospectSize = 100_000
	defaultIntrospectTTL  = 30 * time.Second
)

// NewIntrospectCache 构造。size/ttl<=0 取默认（100k / 30s）。
//
// hook 用于 metrics 埋点，可以 nil（dev / 单元测试）。
func NewIntrospectCache(size int, ttl time.Duration, hook IntrospectCacheHook) *IntrospectCache {
	if size <= 0 {
		size = defaultIntrospectSize
	}
	if ttl <= 0 {
		ttl = defaultIntrospectTTL
	}
	c := &IntrospectCache{
		hook: hook,
		ttl:  ttl,
	}
	// userIndex 容量等量；TTL 比主缓存稍长，避免主缓存还在但索引被淘汰。
	c.userIndex = expirable.NewLRU[int64, map[string]struct{}](size, nil, ttl*2)
	c.entries = expirable.NewLRU[string, IntrospectCacheValue](size, c.onEvict, ttl)
	return c
}

// onEvict 主缓存淘汰一条时同步删 userIndex 里的引用，避免索引膨胀。
func (c *IntrospectCache) onEvict(key string, val IntrospectCacheValue) {
	if val.UserID == 0 {
		return
	}
	if set, ok := c.userIndex.Get(val.UserID); ok {
		delete(set, key)
		if len(set) == 0 {
			c.userIndex.Remove(val.UserID)
		}
		// 同 user 仍有其他 token 时把更新后的 set 写回（expirable Get 不刷新值）。
	}
}

// hashKey 把 jwt 原文 hash 为定长 hex 串作为缓存 key。
// 用 sha256 而不是 sha1：抗碰撞 + 抗预镜像（虽然 jwt 本身已是高熵串，多一层防御）。
func hashKey(jwt string) string {
	if jwt == "" {
		return ""
	}
	h := sha256.Sum256([]byte(jwt))
	return hex.EncodeToString(h[:])
}

// Get 命中且 ExpiresAt 还在未来时返回缓存值。已过期视为 miss（不返过期 token）。
func (c *IntrospectCache) Get(jwt string) (IntrospectCacheValue, bool) {
	if jwt == "" {
		return IntrospectCacheValue{}, false
	}
	v, ok := c.entries.Get(hashKey(jwt))
	if c.hook != nil {
		// 注意：过期视为 miss，hook 也按 miss 计数（让命中率反映"实际可用"）。
		c.hook.Lookup(ok && time.Now().Before(v.ExpiresAt))
	}
	if !ok {
		return IntrospectCacheValue{}, false
	}
	if !time.Now().Before(v.ExpiresAt) {
		// JWT 已过期，主动驱逐避免 stale 命中。
		c.entries.Remove(hashKey(jwt))
		return IntrospectCacheValue{}, false
	}
	// 返副本，外部改 Permissions 不污染缓存。
	cp := v
	if len(v.Permissions) > 0 {
		cp.Permissions = append([]string(nil), v.Permissions...)
	}
	if len(v.Scopes) > 0 {
		cp.Scopes = append([]string(nil), v.Scopes...)
	}
	return cp, true
}

// Put 存条目。
//   - jwt 为空 / val.UserID==0 / val.ExpiresAt 已过 -> 忽略。
//   - 不复制传入的 Permissions/Scopes（caller 不应再改）。
func (c *IntrospectCache) Put(jwt string, val IntrospectCacheValue) {
	if jwt == "" || val.UserID == 0 {
		return
	}
	now := time.Now()
	if !now.Before(val.ExpiresAt) {
		return
	}
	val.CachedAt = now
	key := hashKey(jwt)
	c.entries.Add(key, val)

	// 更新 user → keys 索引，给 InvalidateUser 用。
	set, ok := c.userIndex.Get(val.UserID)
	if !ok {
		set = make(map[string]struct{}, 4)
	}
	set[key] = struct{}{}
	c.userIndex.Add(val.UserID, set)
}

// Invalidate 删一条（Logout 入口调）。
func (c *IntrospectCache) Invalidate(jwt string) {
	if jwt == "" {
		return
	}
	c.entries.Remove(hashKey(jwt))
	// onEvict 不会触发（Remove 不调 evict），手动同步索引是不必要的：
	// 索引项的下一次访问会发现 set 里指向的 key 已不在主缓存 → 自然 miss。
}

// InvalidateUser 删该 user 的所有缓存条目（LogoutAll / 改密码 / 风控冻结时调）。
// O(k)，k = 该 user 当前缓存的 token 数（典型 <=10：多设备登录）。
func (c *IntrospectCache) InvalidateUser(userID int64) {
	set, ok := c.userIndex.Get(userID)
	if !ok {
		return
	}
	for key := range set {
		c.entries.Remove(key)
	}
	c.userIndex.Remove(userID)
}

// Purge 清空所有条目（紧急 break-glass / 测试用）。
func (c *IntrospectCache) Purge() {
	c.entries.Purge()
	c.userIndex.Purge()
}

// Len 返当前条目数（指标 / 调试用）。
func (c *IntrospectCache) Len() int {
	if c == nil || c.entries == nil {
		return 0
	}
	return c.entries.Len()
}

// Package store 定义风控引擎需要的存储接口：计数器 + 黑名单。
//
// 内存实现足够 dev / 小规模生产；大规模走 Redis 只需替换实现。
package store

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// normalizeBlacklistKey 把 dimension / value 归一化，避免 case + whitespace
// 绕过：黑名单加 "user123"，攻击者发 "User123" / " user123 " / "USER123"
// 都不命中。生产 Redis 实现也应在写 / 读两侧使用同一归一化。
func normalizeBlacklistKey(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// IntervalTracker 记录"key 上次见到的时间"。给 register_interval / cross_city_login 等
// "距上次操作 < N 秒"类规则用。
//
// 区别于 Counter (聚合计数) 和 LinkStore (关联图)：本接口只记 last-seen-time，
// O(1) 读写，跟 LinkStore 一样有 TTL 过期。
type IntervalTracker interface {
	// LastSeen 上次记录的时间戳；不存在 / 已过期返回零值。
	LastSeen(ctx context.Context, key string) time.Time
	// Record 把 now 写到 key（原子）。前一次时间作为返回值（零值 = 第一次）。
	Record(ctx context.Context, key string, now time.Time) time.Time
}

// Counter 交易累计计数器（日 / 月 / 滑窗速率 / 滚动 N 天）
type Counter interface {
	// GetDaily 获取 key 今日累计金额（minor unit）
	GetDaily(ctx context.Context, key string) int64
	// GetMonthly 获取 key 本月累计金额
	GetMonthly(ctx context.Context, key string) int64
	// GetVelocity 获取 key 在最近 windowMin 分钟内的交易笔数
	GetVelocity(ctx context.Context, key string, windowMin int) int
	// GetVelocityAmount 滑窗 windowMin 分钟内的累计金额（minor unit）。
	// velocity_amount 规则用：N 分钟内同 key 累计金额过大 = 突发风险（分散式 fraud 测金额上限）。
	GetVelocityAmount(ctx context.Context, key string, windowMin int) int64
	// GetRollingDays 滚动 N 天累计金额（含今天，UTC 自然日聚合）。
	// 用日级回放：rolling_days 规则（"过去 7 天累计被拒 > 5 万"）的实现源。
	// N <= 0 返 0；超过实现保留期被裁。
	GetRollingDays(ctx context.Context, key string, days int) int64
	// Incr 上报一笔成功交易（Report 调用）
	Incr(ctx context.Context, key string, amount int64)
	// Purge 清掉给定 key 的所有 daily / monthly / velocity 计数（GDPR right-to-erasure）。
	// 返回清掉的桶数（粗粒度）。重复调幂等。
	Purge(ctx context.Context, key string) int
}

// Blacklist 黑名单存储
type Blacklist interface {
	// Contains 判定 (dimension, value) 是否在黑名单
	Contains(ctx context.Context, dimension, value string) bool
	// Add 添加黑名单条目
	Add(ctx context.Context, dimension, value, reason string)
	// Remove 移除
	Remove(ctx context.Context, dimension, value string)
	// List 按维度列出
	List(ctx context.Context, dimension string) []BlacklistEntry
}

type BlacklistEntry struct {
	Dimension string
	Value     string
	Reason    string
	AddedAt   time.Time
}

// ─── 内存实现 ───────────────────────────────────────────────────────

// MemCounter 基于 sync.Map 的内存计数器。进程重启清零。
//
// daily map 同时是 GetRollingDays 的回放源：旧 daily key 不会自动 GC，进程重启
// 才清。生产用 Redis 实现走 EXPIREAT，30 天自动过期，本结构主要给单测 / dev 用。
type MemCounter struct {
	daily    sync.Map // "YYYYMMDD:key" → *atomic.Int64
	monthly  sync.Map // "YYYYMM:key"   → *atomic.Int64
	velocity sync.Map // "key" → *velocityWindow
}

func NewMemCounter() *MemCounter { return &MemCounter{} }

func (c *MemCounter) GetDaily(_ context.Context, key string) int64 {
	k := time.Now().UTC().Format("20060102") + ":" + key
	if v, ok := c.daily.Load(k); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

func (c *MemCounter) GetMonthly(_ context.Context, key string) int64 {
	k := time.Now().UTC().Format("200601") + ":" + key
	if v, ok := c.monthly.Load(k); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// GetRollingDays 把最近 N 天（含今天，UTC）的 daily 累计加和。
// N <= 0 返 0。Mem 实现没有保留期上限，调用方应自约束（通常 <= 30）。
func (c *MemCounter) GetRollingDays(_ context.Context, key string, days int) int64 {
	if days <= 0 {
		return 0
	}
	now := time.Now().UTC()
	var sum int64
	for i := 0; i < days; i++ {
		k := now.AddDate(0, 0, -i).Format("20060102") + ":" + key
		if v, ok := c.daily.Load(k); ok {
			sum += v.(*atomic.Int64).Load()
		}
	}
	return sum
}

func (c *MemCounter) GetVelocity(_ context.Context, key string, windowMin int) int {
	v, ok := c.velocity.Load(key)
	if !ok {
		return 0
	}
	w := v.(*velocityWindow)
	if windowMin <= 0 {
		windowMin = 1
	}
	if windowMin > velocityBuckets {
		windowMin = velocityBuckets
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// 当前分钟 + 过去 windowMin-1 分钟的桶相加。expireOldBuckets 同步把过期的清零，
	// 确保 GetVelocity 不读到 60min 之前的脏数据。
	now := time.Now()
	w.expireOldBuckets(now)
	count := 0
	curMinute := now.Unix() / 60
	for i := 0; i < windowMin; i++ {
		minute := curMinute - int64(i)
		idx := int(minute % int64(velocityBuckets))
		if idx < 0 {
			idx += velocityBuckets
		}
		// 仅在桶时间戳匹配时计入；过期桶已被 expire 清零
		if w.bucketStart[idx] == minute {
			count += w.bucketCount[idx]
		}
	}
	return count
}

func (c *MemCounter) Incr(_ context.Context, key string, amount int64) {
	// daily / monthly atomic add：曾用 LoadOrStore + val.Store(amount)，存在 race ——
	//   T1: LoadOrStore 插入 val_t1（loaded=false）
	//   T2: LoadOrStore 拿到 val_t1（loaded=true），val_t1.Add(50) → 50
	//   T1: val.Store(amount=100) → 覆盖了 T2 的 50，最终 100 而非 150
	// 修法：所有路径都走 Add；初始化的 atomic.Int64 零值 = 0，Add(amount) 等价于
	// 累计的第一笔。LoadOrStore 之后无论 loaded 与否，都对 *返回的* 那个 val 调 Add。
	dk := time.Now().UTC().Format("20060102") + ":" + key
	dval, _ := c.daily.LoadOrStore(dk, new(atomic.Int64))
	dval.(*atomic.Int64).Add(amount)

	mk := time.Now().UTC().Format("200601") + ":" + key
	mval, _ := c.monthly.LoadOrStore(mk, new(atomic.Int64))
	mval.(*atomic.Int64).Add(amount)
	// velocity 走时间分桶。与原 []time.Time 实现等价语义但 O(1)：
	//   - bucketStart[i] = 该桶代表的分钟（minutes since epoch）；
	//     负数 / 不匹配 = 桶过期。
	//   - bucketCount[i] = 该分钟的命中次数。
	// GetVelocity(windowMin) 只需把当前 + 过去 windowMin-1 个桶相加。
	// 内存上限：每个 key 60 桶 = 60×16B = 1KB；远低于原 []time.Time 在突发流量下
	// 的几十 KB。
	vv, _ := c.velocity.LoadOrStore(key, newVelocityWindow())
	w := vv.(*velocityWindow)
	w.mu.Lock()
	now := time.Now()
	w.expireOldBuckets(now)
	curMinute := now.Unix() / 60
	idx := int(curMinute % int64(velocityBuckets))
	if idx < 0 {
		idx += velocityBuckets
	}
	if w.bucketStart[idx] != curMinute {
		// 该 slot 上次属于 60 分钟之前的同 idx 周期，已被 expireOldBuckets 清零
		w.bucketStart[idx] = curMinute
		w.bucketAmount[idx] = 0
	}
	w.bucketCount[idx]++
	w.bucketAmount[idx] += amount
	w.mu.Unlock()
}

// GetVelocityAmount 滑窗内累计金额。实现：把 windowMin 个桶的 amount 加起来。
func (c *MemCounter) GetVelocityAmount(_ context.Context, key string, windowMin int) int64 {
	v, ok := c.velocity.Load(key)
	if !ok {
		return 0
	}
	w := v.(*velocityWindow)
	if windowMin <= 0 {
		windowMin = 1
	}
	if windowMin > velocityBuckets {
		windowMin = velocityBuckets
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	curMinute := time.Now().Unix() / 60
	var sum int64
	for back := 0; back < windowMin; back++ {
		minute := curMinute - int64(back)
		idx := int(minute % int64(velocityBuckets))
		if idx < 0 {
			idx += velocityBuckets
		}
		if w.bucketStart[idx] == minute {
			sum += w.bucketAmount[idx]
		}
	}
	return sum
}

// velocityBuckets 时间桶大小（= velocity 最大可查询窗口分钟数）。固定 60 让
// GetVelocity(windowMin <= 60) 都能精确回答；超过 60 的 windowMin 在
// GetVelocity 里被裁到 60。
const velocityBuckets = 60

// velocityWindow 单 key 的时间分桶滑窗。互斥锁仅保护两个数组的一致性更新，
// 锁粒度极小（O(1) 操作），即使在每秒数千请求下争用也可忽略。
type velocityWindow struct {
	mu           sync.Mutex
	bucketStart  [velocityBuckets]int64 // 该桶代表的"分钟数"；不匹配即过期
	bucketCount  [velocityBuckets]int
	bucketAmount [velocityBuckets]int64 // 该分钟累计金额，用于 GetVelocityAmount
}

func newVelocityWindow() *velocityWindow { return &velocityWindow{} }

// expireOldBuckets 把 now-velocityBuckets 之前的桶清零。使其不被 GetVelocity
// 误算。amortized O(velocityBuckets) = O(60)，常数。
func (w *velocityWindow) expireOldBuckets(now time.Time) {
	curMinute := now.Unix() / 60
	cutoff := curMinute - int64(velocityBuckets)
	for i := 0; i < velocityBuckets; i++ {
		if w.bucketStart[i] <= cutoff {
			w.bucketStart[i] = 0
			w.bucketCount[i] = 0
			w.bucketAmount[i] = 0
		}
	}
}

// MemIntervalTracker 内存版 IntervalTracker。生产换 Redis SET key value EX TTL。
//
// TTL 24 小时（typical "距上次操作"窗口）；超时的 LastSeen 返回零值。
type MemIntervalTracker struct {
	mu      sync.RWMutex
	entries map[string]time.Time
	ttl     time.Duration
}

func NewMemIntervalTracker() *MemIntervalTracker {
	return &MemIntervalTracker{entries: make(map[string]time.Time), ttl: 24 * time.Hour}
}

func (t *MemIntervalTracker) LastSeen(_ context.Context, key string) time.Time {
	if key == "" {
		return time.Time{}
	}
	t.mu.RLock()
	v, ok := t.entries[key]
	t.mu.RUnlock()
	if !ok {
		return time.Time{}
	}
	if !v.IsZero() && time.Since(v) > t.ttl {
		t.mu.Lock()
		delete(t.entries, key)
		t.mu.Unlock()
		return time.Time{}
	}
	return v
}

func (t *MemIntervalTracker) Record(_ context.Context, key string, now time.Time) time.Time {
	if key == "" {
		return time.Time{}
	}
	t.mu.Lock()
	prev := t.entries[key]
	t.entries[key] = now
	t.mu.Unlock()
	return prev
}

// MemBlacklist 内存黑名单
type MemBlacklist struct {
	mu      sync.RWMutex
	entries map[string]BlacklistEntry // "dimension:value" → entry
}

func NewMemBlacklist() *MemBlacklist {
	return &MemBlacklist{entries: make(map[string]BlacklistEntry)}
}

// blKey 构造 (dimension, value) 的存储键。Add / Contains / Remove 都走它，
// 保证写读一致。归一化在这里集中处理。
func blKey(dim, val string) string {
	return normalizeBlacklistKey(dim) + ":" + normalizeBlacklistKey(val)
}

func (b *MemBlacklist) Contains(_ context.Context, dimension, value string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.entries[blKey(dimension, value)]
	return ok
}

func (b *MemBlacklist) Add(_ context.Context, dimension, value, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// entry 里的 Dimension / Value 也存归一化形式：list 出来给 admin 看时统一，
	// 同时避免相同语义的多条记录 ("User123" + "user123") 共存导致 Remove
	// 漏删。
	dn := normalizeBlacklistKey(dimension)
	vn := normalizeBlacklistKey(value)
	b.entries[blKey(dimension, value)] = BlacklistEntry{
		Dimension: dn, Value: vn, Reason: reason, AddedAt: time.Now(),
	}
}

func (b *MemBlacklist) Remove(_ context.Context, dimension, value string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, blKey(dimension, value))
}

func (b *MemBlacklist) List(_ context.Context, dimension string) []BlacklistEntry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	dn := normalizeBlacklistKey(dimension)
	var out []BlacklistEntry
	for _, e := range b.entries {
		if e.Dimension == dn {
			out = append(out, e)
		}
	}
	return out
}

// Purge 清 key 的所有 daily / monthly / velocity 计数。GDPR right-to-erasure 用。
// 返回 purge 掉的 daily + monthly + velocity-bucket 总数（粗粒度）。
func (c *MemCounter) Purge(_ context.Context, key string) int {
	if key == "" {
		return 0
	}
	purged := 0
	c.daily.Range(func(k, _ any) bool {
		ks, _ := k.(string)
		// daily key 形如 "YYYYMMDD:<key>"；后缀匹配
		if strings.HasSuffix(ks, ":"+key) {
			c.daily.Delete(k)
			purged++
		}
		return true
	})
	c.monthly.Range(func(k, _ any) bool {
		ks, _ := k.(string)
		if strings.HasSuffix(ks, ":"+key) {
			c.monthly.Delete(k)
			purged++
		}
		return true
	})
	if _, ok := c.velocity.LoadAndDelete(key); ok {
		purged++
	}
	return purged
}

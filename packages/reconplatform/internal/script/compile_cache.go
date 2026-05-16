// compile_cache.go — 编译产物 LRU 缓存.
//
// 场景:
//   - Admin web dry-run: 同一段代码连续调试,每次 RunCode 都重 Compile (5-50ms/次)
//   - Scheduler 定时跑同一规则: 每分钟一次,每次重 Compile 浪费 CPU
//   - Batch run: N 条规则 × M 次窗口,大量重复编译
//
// 设计:
//   - 双键 cache: 优先按 scriptID (最常见),fallback 按 code hash (同代码改 ID 也复用)
//   - 软上限 128 条 entries (Starlark Program 内存 ~ 几十 KB,128 × 50 KB = 6 MB 可接受)
//   - 命中率 metrics 暴露给 Prometheus
package script

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
)

// CompileCache LRU 缓存 CompiledScript.
//
// 线程安全:get/put 持锁;hit/miss 计数走 atomic.
type CompileCache struct {
	maxEntries int

	mu        sync.Mutex
	byID      map[string]*list.Element // scriptID 主键
	byHash    map[string]*list.Element // code-sha256 二级键 (同代码不同 ID 复用)
	lru       *list.List               // 元素是 *cacheEntry,LRU 顺序

	// metrics
	hits    atomic.Int64
	misses  atomic.Int64
	evicted atomic.Int64
}

type cacheEntry struct {
	id   string
	hash string
	cs   *CompiledScript
}

// NewCompileCache 构造. maxEntries <= 0 → 128.
func NewCompileCache(maxEntries int) *CompileCache {
	if maxEntries <= 0 {
		maxEntries = 128
	}
	return &CompileCache{
		maxEntries: maxEntries,
		byID:       map[string]*list.Element{},
		byHash:     map[string]*list.Element{},
		lru:        list.New(),
	}
}

// Get 优先按 id 找,没找到按 hash 找;命中后 move to front.
func (c *CompileCache) Get(scriptID, code string) (*CompiledScript, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if scriptID != "" {
		if el, ok := c.byID[scriptID]; ok {
			ent := el.Value.(*cacheEntry)
			// 同 ID 但 code 变了 → 老 entry 失效, 视为 miss
			if ent.hash != hashCode(code) {
				c.removeElement(el)
				c.misses.Add(1)
				return nil, false
			}
			c.lru.MoveToFront(el)
			c.hits.Add(1)
			return ent.cs, true
		}
	}
	if h := hashCode(code); h != "" {
		if el, ok := c.byHash[h]; ok {
			c.lru.MoveToFront(el)
			c.hits.Add(1)
			return el.Value.(*cacheEntry).cs, true
		}
	}
	c.misses.Add(1)
	return nil, false
}

// Put 插入/更新.
func (c *CompileCache) Put(scriptID, code string, cs *CompiledScript) {
	if cs == nil {
		return
	}
	h := hashCode(code)
	c.mu.Lock()
	defer c.mu.Unlock()

	// 同 ID 的老 entry 先驱逐
	if scriptID != "" {
		if el, ok := c.byID[scriptID]; ok {
			c.removeElement(el)
		}
	}
	if el, ok := c.byHash[h]; ok {
		c.removeElement(el)
	}

	ent := &cacheEntry{id: scriptID, hash: h, cs: cs}
	el := c.lru.PushFront(ent)
	if scriptID != "" {
		c.byID[scriptID] = el
	}
	c.byHash[h] = el

	// 超过上限 → 从尾部驱逐
	for c.lru.Len() > c.maxEntries {
		tail := c.lru.Back()
		if tail != nil {
			c.removeElement(tail)
			c.evicted.Add(1)
		}
	}
}

// Invalidate 显式失效 (规则被删时调).
func (c *CompileCache) Invalidate(scriptID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[scriptID]; ok {
		c.removeElement(el)
	}
}

// removeElement 删除一个 cache 元素 (调用方持锁).
func (c *CompileCache) removeElement(el *list.Element) {
	ent := el.Value.(*cacheEntry)
	if ent.id != "" {
		delete(c.byID, ent.id)
	}
	delete(c.byHash, ent.hash)
	c.lru.Remove(el)
}

// Stats 取计数 (Prometheus 抓).
type CompileCacheStats struct {
	Hits    int64
	Misses  int64
	Evicted int64
	Size    int
	MaxSize int
}

// Stats 暴露.
func (c *CompileCache) Stats() CompileCacheStats {
	c.mu.Lock()
	size := c.lru.Len()
	c.mu.Unlock()
	return CompileCacheStats{
		Hits:    c.hits.Load(),
		Misses:  c.misses.Load(),
		Evicted: c.evicted.Load(),
		Size:    size,
		MaxSize: c.maxEntries,
	}
}

// HitRate 0..1.
func (c *CompileCache) HitRate() float64 {
	h := c.hits.Load()
	m := c.misses.Load()
	if h+m == 0 {
		return 0
	}
	return float64(h) / float64(h+m)
}

// hashCode 算 code 的 sha256[:16] (够用 + 紧凑).
func hashCode(code string) string {
	if code == "" {
		return ""
	}
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:8]) // 16 字节 hex,碰撞概率 < 2^-64
}

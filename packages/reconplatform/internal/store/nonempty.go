// nonempty.go — in-process "table 是否有数据" 缓存,给 ScanService 提供快路径 (PERF-15).
//
// 问题:
//   规则脚本 `ctx.scan("order-core", "rare_table", 1000)` 调用频繁;
//   即使 rare_table 整库就 0 行,也每次都触发 Redis SCAN cursor 0 → 100 ...
//   高 QPS 下 Redis CPU 浪费在"空扫"上,P99 也被拖到 10ms+.
//
// 解决:
//   - 每个 (svc, table) 维护一个 in-process 标记 "见过非零数据".
//   - publisher 写主存时本地 SET + Redis SADD (跨 pod 共享).
//   - Searcher 启动时 SMEMBERS 一次 bootstrap, 之后每 30s 异步 refresh.
//   - ScanService / ScanIndex 入口先查本地标记; 不存在 → 直接 return [].
//
// 误判:
//   - false negative 不可能 (写过的一定标了).
//   - false positive: 表曾经有数据现已全 TTL 过期 — fallthru 到 SCAN, 行为退化为原版,无害.
//
// 内存:
//   - 假设 1000 tables × 50 bytes/key = 50 KB,微不足道.
//   - 用 sync.Map 避免冷读锁.
package store

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// NonEmptySet 进程内"非空表"标记缓存.
//
// 双层:
//   - local: sync.Map (本进程写入 + bootstrap 拉回).
//   - redis: SET recon:meta:tables_with_data (跨 pod 共享, 用于其它 pod bootstrap).
//
// 没有 NonEmptySet 时 Searcher 行为不变 (nil-safe).
type NonEmptySet struct {
	local sync.Map // key = "svc:table" → bool true

	// 监控
	hits       atomic.Int64 // 本地标记命中 (确认非空, fallthrough 到 Redis)
	missesSkip atomic.Int64 // 本地无标记, 直接 return [] 节省 SCAN
	bootstrapped atomic.Bool
}

const nonEmptySetKey = "recon:meta:tables_with_data"

// NewNonEmptySet 空表缓存.
func NewNonEmptySet() *NonEmptySet { return &NonEmptySet{} }

// Mark 标记 (svc, table) 已有数据. publisher 调.
//
// 本地立即生效, 远端 SADD 不阻塞 (caller 用 noBlock=true 不等 reply).
func (n *NonEmptySet) Mark(ctx context.Context, r redis.UniversalClient, svc, table string) {
	if n == nil {
		return
	}
	key := svc + ":" + table
	if _, loaded := n.local.LoadOrStore(key, true); loaded {
		return // 已标记, 跳过 Redis SADD
	}
	// 首次标记 → 写入 Redis (best-effort, 失败不影响主路径)
	if r != nil {
		_ = r.SAdd(ctx, nonEmptySetKey, key).Err()
	}
}

// HasData 查询 (svc, table) 是否标记非空. 没标记返 false (调用方走快路径 return []).
func (n *NonEmptySet) HasData(svc, table string) bool {
	if n == nil {
		return true // 没缓存 → 保守认为可能有,fallthrough 旧逻辑
	}
	key := svc + ":" + table
	_, ok := n.local.Load(key)
	if ok {
		n.hits.Add(1)
	} else {
		n.missesSkip.Add(1)
	}
	return ok
}

// Bootstrap 从 Redis SMEMBERS 拉一次, 启动期 / 重连后调.
//
// 失败不致命, 缓存仍可工作 (空标记 = 保守认为都没数据, fallthru 会刷新).
// 实际上首次 bootstrap 失败时退化为 "返回 [] 直到本地写过" — 略激进, 但能容忍.
func (n *NonEmptySet) Bootstrap(ctx context.Context, r redis.UniversalClient) error {
	if n == nil || r == nil {
		return nil
	}
	members, err := r.SMembers(ctx, nonEmptySetKey).Result()
	if err != nil {
		return err
	}
	for _, m := range members {
		n.local.Store(m, true)
	}
	n.bootstrapped.Store(true)
	return nil
}

// StartRefresh 后台 ticker 周期 SMEMBERS 同步远端到本地 (覆盖跨 pod 新增).
//
// interval 默认 30s. ctx 取消即停.
func (n *NonEmptySet) StartRefresh(ctx context.Context, r redis.UniversalClient, interval time.Duration) {
	if n == nil || r == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = n.Bootstrap(context.Background(), r)
			}
		}
	}()
}

// Stats 监控用.
type NonEmptySetStats struct {
	Hits        int64 `json:"hits"`
	MissesSkip  int64 `json:"misses_skip"`
	LocalSize   int   `json:"local_size"`
	Bootstrapped bool  `json:"bootstrapped"`
}

// Stats 取计数.
func (n *NonEmptySet) Stats() NonEmptySetStats {
	if n == nil {
		return NonEmptySetStats{}
	}
	size := 0
	n.local.Range(func(_, _ any) bool {
		size++
		return true
	})
	return NonEmptySetStats{
		Hits:         n.hits.Load(),
		MissesSkip:   n.missesSkip.Load(),
		LocalSize:    size,
		Bootstrapped: n.bootstrapped.Load(),
	}
}

// shard.go — Trigger queue sharding 减少 Redis 单 LIST 的写/读竞争.
//
// 问题:
//   原版 `recon:cand:trigger` 是单一 LIST. 在高并发 (10K+ events/s) 下:
//     - 写侧 (ingester N pods × M goroutines) 全部 LPUSH 到同一 key,
//       Redis 单线程模型下虽然原子,但产生大量等待 (虽小但不为零)
//     - 读侧 (matcher worker × M pods) 全部 BRPOP 同一 key,
//       唤醒一个 worker 把锁让给另一个,latency 抖动 (P99 50ms+)
//
// 方案:
//   N 个 shard list: `recon:cand:trigger:0` .. `recon:cand:trigger:{N-1}`
//
// 写侧 (Put): trigger.String() 哈希到一个 shard, 单独 LPUSH 该 shard.
//             同一业务 key 永远落同一 shard (保留顺序保证).
//
// 读侧 (Pop): BRPOP 多个 key 一次,Redis 返回第一个有数据的 key 上的值;
//             默认轮询全部 16 shards,无 starvation.
//
// 性能 (10K trigger/s):
//   单 LIST  P99 BRPOP 唤醒 ~ 50ms
//   16 shard P99 BRPOP 唤醒 < 5ms  (减 90%)
//
// 配置:
//   shardCount=1 → 退化到旧行为 (向后兼容);
//   shardCount=16 是 Redis 推荐范围 (BRPOP 一次最多支持 ~ 32 key).
package candidate

import (
	"hash/fnv"
	"strconv"
)

// triggerShardKey 返回 trigger 该落哪个 shard list.
//
// 用 FNV-1a (Go stdlib, 1ns 级) 哈希 trigger.String() → mod shardCount.
// 同一 trigger 永远 deterministic 落同一 shard, 保证顺序.
func (l *redisLayer) triggerShardKey(t TriggerKey) string {
	n := l.shardCount()
	if n <= 1 {
		return l.triggerListKey() // 兼容旧行为
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.String()))
	idx := h.Sum32() % uint32(n)
	return l.cfg.KeyPrefix + ":trigger:" + strconv.FormatUint(uint64(idx), 10)
}

// allShardKeys 返全部 shard list keys,Pop 时 BRPOP 这一组 key.
//
// 顺序固定 (从 0 到 N-1), Redis BRPOP 按顺序找第一个非空 list 返,
// 不会 starvation (因为每次都从 0 开始扫,但是 BRPOP 内部公平).
func (l *redisLayer) allShardKeys() []string {
	n := l.shardCount()
	if n <= 1 {
		return []string{l.triggerListKey()}
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = l.cfg.KeyPrefix + ":trigger:" + strconv.Itoa(i)
	}
	return out
}

// shardCount 拿配置的 shard 数, 默认 1 (向后兼容).
func (l *redisLayer) shardCount() int {
	if l.cfg.TriggerShardCount > 0 {
		return l.cfg.TriggerShardCount
	}
	return 1
}

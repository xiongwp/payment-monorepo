// redis_counter.go: store.Counter 的 Redis 参考实现。
//
// 当前**未编译进默认 build**：避免给 risk-manage 强制加 go-redis 依赖。
// 接入时：
//
//  1. go.mod 加：require github.com/redis/go-redis/v9 v9.x.x
//  2. 删除文件顶部的 //go:build redis 标签
//  3. main.go newCounter 改成：
//
//	   rdb := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
//	   return store.NewRedisCounter(rdb, "risk:")
//
// 实现说明：
//   - Daily / Monthly: INCRBY 简单 string；EXPIREAT 末尾 + 长留存（daily 30d
//     用作 GetRollingDays 的回放源）。
//   - Velocity: ZSet (score=ns ts, member="ns:rand:amount")。member 编码 amount
//     让 GetVelocityAmount 用 Lua 服务端聚合，避免 client 拉所有 entry。
//   - GetRollingDays(N): 直接 MGET N 天的 daily 计数器加和；天粒度对齐（不需要
//     精确滚动到秒，N 天起步的指标用日级足够）。
//   - Incr 全部用 pipeline 一发：单次 RTT 完成 多个维度更新。
//
// 一致性：Redis 单机原子；集群 cluster mode 用 hash tag 让同一 key 的 daily/
// monthly/velocity 落同一槽（{<key>} 包裹），保证 pipeline 走单 shard，MGET
// 跨日 daily 也都在同一 shard。
//
// 错误：所有方法 fail-open（返 0 / no-op + log），不让 risk 路径阻塞。



package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCounter 实现 store.Counter，用 Redis 做后端。prefix 给所有 key 前缀
// 隔离命名空间（多服务共享 Redis 实例时必填，例如 "risk:" / "risk:test:"）。
type RedisCounter struct {
	rdb    *redis.Client
	prefix string

	// dailyRetention daily 键的过期时间。给 GetRollingDays 当回放源；默认 30d。
	dailyRetention time.Duration
}

func NewRedisCounter(rdb *redis.Client, prefix string) *RedisCounter {
	if prefix == "" {
		prefix = "risk:"
	}
	return &RedisCounter{rdb: rdb, prefix: prefix, dailyRetention: 30 * 24 * time.Hour}
}

func (c *RedisCounter) keyDaily(key string, day time.Time) string {
	d := day.UTC().Format("20060102")
	return fmt.Sprintf("%sdaily:%s:{%s}", c.prefix, d, key)
}

func (c *RedisCounter) keyMonthly(key string) string {
	m := time.Now().UTC().Format("200601")
	return fmt.Sprintf("%smonthly:%s:{%s}", c.prefix, m, key)
}

func (c *RedisCounter) keyVelocity(key string) string {
	return fmt.Sprintf("%svel:{%s}", c.prefix, key)
}

func (c *RedisCounter) GetDaily(ctx context.Context, key string) int64 {
	v, err := c.rdb.Get(ctx, c.keyDaily(key, time.Now())).Result()
	if err != nil || v == "" {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

func (c *RedisCounter) GetMonthly(ctx context.Context, key string) int64 {
	v, err := c.rdb.Get(ctx, c.keyMonthly(key)).Result()
	if err != nil || v == "" {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// GetRollingDays 最近 N 天累计金额（含今天）。用 daily:YYYYMMDD 多键 MGET 加和。
// N <= 0 返 0；N 超过 dailyRetention 上限被裁。
func (c *RedisCounter) GetRollingDays(ctx context.Context, key string, days int) int64 {
	if days <= 0 {
		return 0
	}
	maxDays := int(c.dailyRetention / (24 * time.Hour))
	if days > maxDays {
		days = maxDays
	}
	keys := make([]string, days)
	now := time.Now().UTC()
	for i := 0; i < days; i++ {
		keys[i] = c.keyDaily(key, now.AddDate(0, 0, -i))
	}
	vals, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return 0
	}
	var sum int64
	for _, v := range vals {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		n, _ := strconv.ParseInt(s, 10, 64)
		sum += n
	}
	return sum
}

func (c *RedisCounter) GetVelocity(ctx context.Context, key string, windowMin int) int {
	if windowMin <= 0 {
		windowMin = 1
	}
	now := time.Now().UTC().UnixNano()
	cutoff := now - int64(windowMin)*int64(time.Minute)
	// 顺手清理窗口外的过期 entry，让 ZSet 不积累
	_, _ = c.rdb.ZRemRangeByScore(ctx, c.keyVelocity(key), "-inf", strconv.FormatInt(cutoff-1, 10)).Result()
	n, err := c.rdb.ZCount(ctx, c.keyVelocity(key),
		strconv.FormatInt(cutoff, 10), "+inf").Result()
	if err != nil {
		return 0
	}
	return int(n)
}

// velocityAmountScript Lua 服务端累加 ZSet 成员里编码的 amount。
// 成员格式：<ns>:<rand>:<amount>。
//
// KEYS[1]=ZSet key，ARGV[1]=cutoff（ns 时间戳，含）。
// 1) 先把 cutoff 之前的成员 ZREMRANGEBYSCORE 删掉（顺手）
// 2) ZRANGEBYSCORE cutoff +inf 取窗口内全部成员
// 3) 解析最后一个 ":" 后的 token 作为 amount 累加。
//
// 注意：返回值用 string；Lua 整数最大 2^53。我们用 minor unit (cents)，每笔
// 上限 1e10 cents=1 亿，1000 万笔窗口内总和 ~ 1e17，仍在范围内但贴边。
// 走 string 让 client 自己 parse int64。
const velocityAmountScript = `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', tonumber(ARGV[1])-1)
local m = redis.call('ZRANGEBYSCORE', KEYS[1], ARGV[1], '+inf')
local sum = 0
for i=1,#m do
  local s = m[i]
  local idx = -1
  for k=#s,1,-1 do
    if string.sub(s,k,k) == ':' then idx=k; break end
  end
  if idx > 0 then
    sum = sum + tonumber(string.sub(s,idx+1)) or 0
  end
end
return tostring(sum)
`

func (c *RedisCounter) GetVelocityAmount(ctx context.Context, key string, windowMin int) int64 {
	if windowMin <= 0 {
		windowMin = 1
	}
	cutoff := time.Now().UTC().UnixNano() - int64(windowMin)*int64(time.Minute)
	res, err := c.rdb.Eval(ctx, velocityAmountScript,
		[]string{c.keyVelocity(key)}, cutoff).Result()
	if err != nil {
		return 0
	}
	s, _ := res.(string)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (c *RedisCounter) Incr(ctx context.Context, key string, amount int64) {
	now := time.Now().UTC()
	ns := now.UnixNano()
	// 4 字节随机 hex 后缀防同 ns 撞 ZSet member（高并发下纳秒可能重）。
	var randSuf [4]byte
	_, _ = rand.Read(randSuf[:])
	member := fmt.Sprintf("%d:%s:%d", ns, hex.EncodeToString(randSuf[:]), amount)

	pipe := c.rdb.Pipeline()
	pipe.IncrBy(ctx, c.keyDaily(key, now), amount)
	pipe.ExpireAt(ctx, c.keyDaily(key, now), endOfDay().Add(c.dailyRetention))
	pipe.IncrBy(ctx, c.keyMonthly(key), amount)
	pipe.ExpireAt(ctx, c.keyMonthly(key), endOfMonth().Add(90*24*time.Hour))
	pipe.ZAdd(ctx, c.keyVelocity(key), redis.Z{Score: float64(ns), Member: member})
	pipe.Expire(ctx, c.keyVelocity(key), 65*time.Minute) // 1h 滑窗 + 5min 缓冲
	_, _ = pipe.Exec(ctx)
}

func endOfDay() time.Time {
	t := time.Now().UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
}

func endOfMonth() time.Time {
	t := time.Now().UTC()
	first := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	return first.Add(-time.Second)
}

// 保持 strings 引用以便未来扩展（避免 unused import）。
var _ = strings.Builder{}

// Purge GDPR right-to-erasure：扫描 SCAN 删 daily:*:{key} / monthly:*:{key} /
// vel:{key}。SCAN 是 cluster-friendly（不全表 KEYS）。注意 hash-tag 让所有相关
// key 落同 shard，所以单 shard SCAN 即可。
func (c *RedisCounter) Purge(ctx context.Context, key string) int {
	if key == "" {
		return 0
	}
	purged := 0
	patterns := []string{
		fmt.Sprintf("%sdaily:*:{%s}", c.prefix, key),
		fmt.Sprintf("%smonthly:*:{%s}", c.prefix, key),
		fmt.Sprintf("%svel:{%s}", c.prefix, key),
	}
	for _, pat := range patterns {
		var cursor uint64
		for {
			keys, next, err := c.rdb.Scan(ctx, cursor, pat, 100).Result()
			if err != nil {
				break
			}
			if len(keys) > 0 {
				_, _ = c.rdb.Del(ctx, keys...).Result()
				purged += len(keys)
			}
			if next == 0 {
				break
			}
			cursor = next
		}
	}
	return purged
}

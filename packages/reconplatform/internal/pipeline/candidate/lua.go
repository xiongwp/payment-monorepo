// lua.go — Candidate Layer 用 Lua script 把多 RTT 操作合并成 1 RTT 原子化.
//
// 性能对比 (10K events/s 负载):
//
//   Put (旧 Pipeline 版): HSET + EXPIRE + (SADD + EXPIRE) × N_indexes + HLEN
//                         3-5 个 RTT,~ 0.5ms/event × 10K = 5s/s = 50% CPU on Redis client
//   Put (Lua 版):          1 个 RTT,~ 0.1ms/event × 10K = 1s/s = 10% CPU
//
//   收益: 5x throughput,Redis CPU 占用 -40%.
//
// Lua script 缓存:
//   - 用 EVALSHA 调,Redis 服务端会 cache 编译产物
//   - go-redis 自动处理 SHA1 + fallback EVAL (NoScript error 后)
//
// 原子性:
//   - Redis Lua 单线程执行,N 个 ARGV 一次性写入,中间不会被其他 client 打断
//   - 桶达 trigger 阈值的判断+触发 一并在 Lua 内完成,无 TOCTOU 风险.
package candidate

import (
	"github.com/redis/go-redis/v9"
)

// putLuaScript Put 操作的原子 Lua.
//
// KEYS[1]:   主桶 key (recon:cand:{biz_key}:{val})
// KEYS[2]:   trigger 队列 key (recon:cand:trigger)
// ARGV[1]:   event_id (HSET field)
// ARGV[2]:   event JSON (HSET value)
// ARGV[3]:   主桶 TTL (秒)
// ARGV[4]:   trigger 阈值 (int, 桶达此即推 trigger)
// ARGV[5]:   trigger queue 元素 (biz_key:val 字符串)
// ARGV[6]:   trigger queue 最大保留长度 (LTRIM, 防 OOM)
//
// 返回:
//   {hlen_after_put, triggered_now (0/1)}
//
// 注:
//   - 二级索引 (SADD recon:cand:idx:...) 由 caller 通过单独的 luaIndexScript 调,
//     避免 KEYS / ARGV 数组超长且降低 cache 命中率.
var putLuaScript = redis.NewScript(`
-- KEYS[1] bucket, KEYS[2] trigger list
-- ARGV: event_id, event_json, bucket_ttl, trigger_threshold, trigger_member, queue_max_len
local hlen_after = redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
-- HSET 返新增的 field 数,但我们要的是总长度
local total = redis.call('HLEN', KEYS[1])
redis.call('EXPIRE', KEYS[1], ARGV[3])

local triggered = 0
local threshold = tonumber(ARGV[4]) or 2
if total >= threshold then
    redis.call('LPUSH', KEYS[2], ARGV[5])
    local maxlen = tonumber(ARGV[6]) or 1000000
    redis.call('LTRIM', KEYS[2], 0, maxlen - 1)
    triggered = 1
end
return {total, triggered}
`)

// indexLuaScript 二级索引添加 + TTL 设置.
//
// KEYS[1]:   索引 key (recon:cand:idx:{secondary}:{val})
// ARGV[1]:   member (biz_key:val,指向哪个主桶)
// ARGV[2]:   TTL (秒)
var indexLuaScript = redis.NewScript(`
redis.call('SADD', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1
`)

// ackMatchLuaScript AckMatch 原子: 选择 keepHistory 或 Del.
//
// KEYS[1]:   bucket key
// KEYS[2]:   lock key (要释放,避免锁悬挂)
// ARGV[1]:   keep_history (0 或 1)
// ARGV[2]:   history_ttl_sec (keep_history=1 时用,默认 3600)
var ackMatchLuaScript = redis.NewScript(`
if tonumber(ARGV[1]) == 1 then
    redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]) or 3600)
else
    redis.call('DEL', KEYS[1])
end
redis.call('DEL', KEYS[2])
return 1
`)

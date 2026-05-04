package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// stateTTL ORDER 与 PAYMENT 事件之间的最大时间窗。redis-go 的 Set 第 4 个
// 参数是 time.Duration，传 int(60) 会被当 60 纳秒 → key 立即过期 → reconcile
// 永远不触发 → 任何资金缺漏不会被告警。
//
// 资损修复：把 TTL 调到 1 小时（覆盖正常支付链路 + 重试 / 异步 webhook 慢回的
// 长尾），并改 lua atomic upsert 防 read-modify-write race。
const stateTTL = time.Hour

// upsertScript 原子合并：若 key 不存在 → 写入新 state；存在 → 解码、合并、写回。
// 用 EVAL 跑 Lua 让整个 read-modify-write 在 redis 单线程内原子完成，杜绝两个
// 消费 goroutine 处理同一 OrderID 的 ORDER + PAYMENT 时丢事件。
//
// 输入：KEYS[1]=state_key, ARGV[1]=ttl_seconds, ARGV[2]=event_json (Type/Amount)
// 返回：合并后的 state JSON，或空字符串（state 已 complete 由调用方决定 Del）
//
// 调用方语义：
//   - 拿到的 state 若 .order 和 .payment 都非空 → reconcile 触发，调 Del
//   - 否则继续等下一条事件
var upsertScript = redis.NewScript(`
local cur = redis.call("GET", KEYS[1])
local state
if cur then
  state = cjson.decode(cur)
else
  state = {}
end
local ev = cjson.decode(ARGV[2])
state.updated = tonumber(ARGV[3])
if ev.type == "ORDER" then
  state.order = ev
elseif ev.type == "PAYMENT" then
  state.payment = ev
end
local payload = cjson.encode(state)
redis.call("SET", KEYS[1], payload, "EX", tonumber(ARGV[1]))
return payload
`)

type Store struct {
	rdb *redis.Client
}

func New(addr string) *Store {
	return &Store{
		rdb: redis.NewClient(&redis.Options{Addr: addr}),
	}
}

// Get 读 state；ErrNotFound（key 不存在）由调用方判断走 zero-value 路径还是
// 报错。其它错误透传。
func (s *Store) Get(ctx context.Context, key string, v interface{}) error {
	val, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ErrNotFound
		}
		return err
	}
	return json.Unmarshal([]byte(val), v)
}

// ErrNotFound state 不存在（首次事件或已被 Del）。
var ErrNotFound = errors.New("state not found")

// UpsertEvent 原子合并事件到 state；返回合并后 state JSON，让 caller 检查
// 是否 ORDER+PAYMENT 都到齐。事件 JSON 结构应同 model.Event。
func (s *Store) UpsertEvent(ctx context.Context, key string, eventJSON []byte) (string, error) {
	res, err := upsertScript.Run(ctx, s.rdb, []string{key},
		int(stateTTL.Seconds()), string(eventJSON), time.Now().Unix(),
	).Result()
	if err != nil {
		return "", err
	}
	str, _ := res.(string)
	return str, nil
}

func (s *Store) Del(ctx context.Context, key string) {
	s.rdb.Del(ctx, key)
}

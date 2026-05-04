// Redis Stream 实现：开 build tag `redis` 后才编译进二进制。
//
// 拓扑：
//   - Publish: XADD <prefix><topic> * "data" <json>
//   - Subscribe: XGROUP CREATE → XREADGROUP（block）→ XACK → loop
//   - 失败重投：handler 返 error → 不 XACK → 由 PEL 控制；超过 idle 时长后调用方
//     自己跑 XCLAIM 接管。本实现不主动 claim，留给运维 worker 处理（典型：
//     起一个 stale-claim job 周期 XPENDING + XCLAIM）。

//go:build redis

package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisBus struct {
	rdb        *redis.Client
	prefix     string
	maxLen     int64 // XADD MAXLEN ~ 限制 stream 长度，防爆 Redis；默认 100k
	blockMS    int64 // XREADGROUP BLOCK 毫秒数；默认 5000
}

func NewRedisBus(rdb *redis.Client, prefix string) *RedisBus {
	if prefix == "" {
		prefix = "risk:bus:"
	}
	return &RedisBus{rdb: rdb, prefix: prefix, maxLen: 100_000, blockMS: 5000}
}

func (b *RedisBus) stream(t Topic) string { return fmt.Sprintf("%s%s", b.prefix, t) }

func (b *RedisBus) Publish(ctx context.Context, ev Event) error {
	if ev.OccurAt.IsZero() {
		ev.OccurAt = time.Now()
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	args := &redis.XAddArgs{
		Stream: b.stream(ev.Topic),
		MaxLen: b.maxLen,
		Approx: true, // ~ MAXLEN，让 Redis 走快路径
		Values: map[string]any{"data": body, "key": ev.Key},
	}
	return b.rdb.XAdd(ctx, args).Err()
}

func (b *RedisBus) Subscribe(ctx context.Context, topic Topic, group, consumer string, h Handler) error {
	stream := b.stream(topic)
	// XGROUP CREATE 幂等：已存在的 group 报错忽略
	_ = b.rdb.XGroupCreateMkStream(ctx, stream, group, "$").Err()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    group,
			Consumer: consumer,
			Streams:  []string{stream, ">"},
			Count:    32,
			Block:    time.Duration(b.blockMS) * time.Millisecond,
		}).Result()
		if err != nil {
			if err == redis.Nil || ctx.Err() != nil {
				continue
			}
			// 转瞬故障 → 短退避；不打死循环
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		for _, s := range res {
			for _, m := range s.Messages {
				ev, err := decodeMessage(m)
				if err != nil {
					// 坏消息 → 直接 ACK 丢掉，避免毒消息阻塞 group。
					_ = b.rdb.XAck(ctx, stream, group, m.ID).Err()
					continue
				}
				if err := h(ctx, ev); err != nil {
					// 不 ACK → PEL 留着；调用方负责 stale-claim
					continue
				}
				_ = b.rdb.XAck(ctx, stream, group, m.ID).Err()
			}
		}
	}
}

func decodeMessage(m redis.XMessage) (Event, error) {
	raw, ok := m.Values["data"].(string)
	if !ok {
		return Event{}, fmt.Errorf("missing data")
	}
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return Event{}, err
	}
	return ev, nil
}

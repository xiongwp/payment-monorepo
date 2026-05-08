package cdc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Publisher 把 cdc.Event 落 Redis：
//
//   1. SET   recon:evt:<svc>:<table>:<pk>     event JSON   EX <ttl>
//   2. SADD  recon:idx:<idx_name>:<value>     "<svc>:<table>:<pk>"
//      （索引也带 TTL：取该事件的 TTL，避免索引比主存活得久）
//   3. XADD  recon:stream:events MAXLEN ~10000  ts svc table pk op
//      （事件驱动脚本订阅消费组 recon-engine）
//   4. (异步) HSET recon:cdc:pos:<svc>  file/pos/gtid
//
// 关键点：
//   - 用 Pipeline，单事件 1 RTT 内全部写完
//   - TTL 来自 TTLProvider（OnChange 热更）
//   - 失败不阻塞 binlog 读取（caller 拿到 err 自己决定是否回退 / 重试）
//   - 索引 TTL 与主存一致：避免主存过期后索引留下死引用（脚本会查到 nil）
//
// Stream MAXLEN ~10000 限长，事件驱动只关心"近期"事件；老事件靠 evt:<key>
// 主存查询。
type Publisher struct {
	r           redis.UniversalClient
	ttl         *TTLProvider
	logger      *zap.Logger
	streamMaxLen int64

	// 指标计数（暴露给 prometheus）
	OnPublish func(svc, table string, op Op, err error)
}

// NewPublisher caller 传入已经 dial 好的 redis client + TTL provider。
// streamMaxLen 0 → 用默认 10000；负值 → 禁用 Stream 写入（仅 SET + 索引）。
func NewPublisher(r redis.UniversalClient, ttl *TTLProvider, logger *zap.Logger) *Publisher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Publisher{
		r:            r,
		ttl:          ttl,
		logger:       logger,
		streamMaxLen: 10000,
	}
}

// Publish 把单条事件写入 Redis（1 RTT pipeline）。
// 调用者已经组装好 Event.Indexes（parser 阶段就填了）。
func (p *Publisher) Publish(ctx context.Context, e *Event) error {
	ttl := p.ttl.Resolve(e.Service, e.Table)
	pipe := p.r.Pipeline()

	// 1) 主存
	pipe.Set(ctx, e.EventKey(), e.JSON(), ttl)

	// 2) 索引（每个 idx_col 写一条 SADD + EXPIRE）。
	//    用 SADD 不是 SET：同一 indexed key 可能引用多条事件。
	//    EXPIRE 不能给 SET 的某成员单独配，所以索引 SET 的 TTL 跟该事件 TTL 走。
	//    后续若同 SET 内有新事件 EXPIRE 会被 refresh 到那条事件的 TTL（合理：取最大）。
	for idxCol, val := range e.Indexes {
		if val == "" {
			continue
		}
		idxKey := fmt.Sprintf("recon:idx:%s:%s", idxCol, val)
		pipe.SAdd(ctx, idxKey, e.MemberRef())
		pipe.Expire(ctx, idxKey, ttl)
	}

	// 3) Stream（事件驱动）
	if p.streamMaxLen > 0 {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: "recon:stream:events",
			MaxLen: p.streamMaxLen,
			Approx: true, // ~MAXLEN，O(1) 修剪
			Values: map[string]any{
				"svc":   e.Service,
				"table": e.Table,
				"pk":    e.PK,
				"op":    string(e.Op),
				"ts":    e.Timestamp.UnixMilli(),
			},
		})
	}

	_, err := pipe.Exec(ctx)
	if p.OnPublish != nil {
		p.OnPublish(e.Service, e.Table, e.Op, err)
	}
	if err != nil {
		// caller 看到 error 自己决定回退；publisher 不重试（避免无界堆积）
		return fmt.Errorf("redis pipeline: %w", err)
	}
	return nil
}

// PublishBatch 批量发布（高 TPS 场景：canal callback 攒一批再 flush）。
// 返第一个错误（中途失败的事件 caller 自己重新发即可，幂等：SET 覆盖、
// SADD 重复成员无副作用、XADD 会写一条新 ID）。
//
// 注意：MaxLen 只对最后一条 XADD 生效（pipeline 不能跨 args 共享 MaxLen），
// 但因为是 Approx 修剪，每次都修一点也 OK。
func (p *Publisher) PublishBatch(ctx context.Context, events []*Event) error {
	if len(events) == 0 {
		return nil
	}
	pipe := p.r.Pipeline()
	for _, e := range events {
		ttl := p.ttl.Resolve(e.Service, e.Table)
		pipe.Set(ctx, e.EventKey(), e.JSON(), ttl)
		for idxCol, val := range e.Indexes {
			if val == "" {
				continue
			}
			idxKey := fmt.Sprintf("recon:idx:%s:%s", idxCol, val)
			pipe.SAdd(ctx, idxKey, e.MemberRef())
			pipe.Expire(ctx, idxKey, ttl)
		}
		if p.streamMaxLen > 0 {
			pipe.XAdd(ctx, &redis.XAddArgs{
				Stream: "recon:stream:events",
				MaxLen: p.streamMaxLen,
				Approx: true,
				Values: map[string]any{
					"svc":   e.Service,
					"table": e.Table,
					"pk":    e.PK,
					"op":    string(e.Op),
					"ts":    e.Timestamp.UnixMilli(),
				},
			})
		}
	}
	_, err := pipe.Exec(ctx)
	if p.OnPublish != nil {
		for _, e := range events {
			p.OnPublish(e.Service, e.Table, e.Op, err)
		}
	}
	return err
}

// SavePosition 持久化 binlog 位点。canal flush 周期调（每秒一次足够），
// 不放进每条事件 pipeline，避免 fsync 风暴。
func (p *Publisher) SavePosition(ctx context.Context, service, file string, pos uint32, gtid string) error {
	key := "recon:cdc:pos:" + service
	return p.r.HSet(ctx, key, map[string]any{
		"file":   file,
		"pos":    pos,
		"gtid":   gtid,
		"saved":  time.Now().UnixMilli(),
	}).Err()
}

// LoadPosition 启动期读上次保存的位点（若返 false 表示没记录，从最新位点起跑）。
func (p *Publisher) LoadPosition(ctx context.Context, service string) (file string, pos uint32, gtid string, ok bool) {
	key := "recon:cdc:pos:" + service
	m, err := p.r.HGetAll(ctx, key).Result()
	if err != nil || len(m) == 0 {
		return "", 0, "", false
	}
	file = m["file"]
	gtid = m["gtid"]
	if v, ok2 := m["pos"]; ok2 {
		var n uint32
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			pos = n
		}
	}
	return file, pos, gtid, file != ""
}

// FormatHumanIdxKey 给 admin web 调试用：把索引 key 拆出来 (idx_name, value)。
func FormatHumanIdxKey(redisKey string) (idxName, value string, ok bool) {
	const p = "recon:idx:"
	if !strings.HasPrefix(redisKey, p) {
		return "", "", false
	}
	rest := strings.TrimPrefix(redisKey, p)
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return "", "", false
	}
	return rest[:colon], rest[colon+1:], true
}

package cdc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"reconcile-system/internal/store"
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

	// PERF-15: 写主存时同步标记 (svc, table) 非空, 给 Searcher.ScanService 快路径.
	// 可选 — nil 即不标记, 老行为.
	nonEmpty *store.NonEmptySet

	// 指标计数（暴露给 prometheus）
	OnPublish func(svc, table string, op Op, err error)
}

// WithNonEmptySet 挂 PERF-15 非空缓存. 每条 Publish 首次见到 (svc, table)
// 时 SADD recon:meta:tables_with_data → 跨 pod 共享.
func (p *Publisher) WithNonEmptySet(n *store.NonEmptySet) *Publisher {
	p.nonEmpty = n
	return p
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
//
// 日志策略（数据流可观察性）：
//
//	debug-level: 写入前/写入后各打一条，含 svc/table/pk/op/idx_count/ttl/dur
//	             生产环境关 debug 即可消音；dev 排查 binlog 是不是真到 Redis
//	             这俩日志最关键。
//	warn-level:  pipeline 失败 + 元信息，给 oncall 排查 Redis 抖动 / 网络问题
func (p *Publisher) Publish(ctx context.Context, e *Event) error {
	ttl := p.ttl.Resolve(e.Service, e.Table)

	// 写入前日志：标记本条事件即将落 Redis（写入前能看到，
	// 即使后面 Exec 卡住也能定位是哪条事件挂了）
	p.logger.Debug("redis publish: begin",
		zap.String("svc", e.Service),
		zap.String("table", e.Table),
		zap.String("pk", e.PK),
		zap.String("op", string(e.Op)),
		zap.Int("idx_count", len(e.Indexes)),
		zap.Duration("ttl", ttl))

	pipe := p.r.Pipeline()

	// 1) 主存
	pipe.Set(ctx, e.EventKey(), e.JSON(), ttl)

	// 2) 索引（每个 idx_col 写一条 SADD + EXPIRE）。
	//    用 SADD 不是 SET：同一 indexed key 可能引用多条事件。
	//    EXPIRE 不能给 SET 的某成员单独配，所以索引 SET 的 TTL 跟该事件 TTL 走。
	//    后续若同 SET 内有新事件 EXPIRE 会被 refresh 到那条事件的 TTL（合理：取最大）。
	idxWritten := 0
	for idxCol, val := range e.Indexes {
		if val == "" {
			continue
		}
		idxKey := fmt.Sprintf("recon:idx:%s:%s", idxCol, val)
		pipe.SAdd(ctx, idxKey, e.MemberRef())
		pipe.Expire(ctx, idxKey, ttl)
		idxWritten++
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

	start := time.Now()
	_, err := pipe.Exec(ctx)
	dur := time.Since(start)
	if p.OnPublish != nil {
		p.OnPublish(e.Service, e.Table, e.Op, err)
	}
	if err != nil {
		// 写入失败 — warn 级，关键信息全 dump 让 oncall 一眼看懂
		p.logger.Warn("redis publish: failed",
			zap.String("svc", e.Service),
			zap.String("table", e.Table),
			zap.String("pk", e.PK),
			zap.String("op", string(e.Op)),
			zap.Int("idx_count", idxWritten),
			zap.Duration("dur", dur),
			zap.Error(err))
		// caller 看到 error 自己决定回退；publisher 不重试（避免无界堆积）
		return fmt.Errorf("redis pipeline: %w", err)
	}

	// 写入成功日志：含耗时让 oncall 看 Redis 健康度（>50ms 就该警觉）
	p.logger.Debug("redis publish: ok",
		zap.String("svc", e.Service),
		zap.String("table", e.Table),
		zap.String("pk", e.PK),
		zap.String("op", string(e.Op)),
		zap.String("event_key", e.EventKey()),
		zap.Int("idx_written", idxWritten),
		zap.Duration("dur", dur))
	// PERF-15: 标记 (svc, table) 非空, 给 Searcher.ScanService 快路径.
	// 首次写时本地 + Redis SADD; 后续只本地 LoadOrStore 命中,零开销.
	if p.nonEmpty != nil {
		p.nonEmpty.Mark(ctx, p.r, e.Service, e.Table)
	}
	return nil
}

// PublishBatch 批量发布（高 TPS 场景：canal callback 攒一批再 flush）。
// 返第一个错误（中途失败的事件 caller 自己重新发即可，幂等：SET 覆盖、
// SADD 重复成员无副作用、XADD 会写一条新 ID）。
//
// 注意：MaxLen 只对最后一条 XADD 生效（pipeline 不能跨 args 共享 MaxLen），
// 但因为是 Approx 修剪，每次都修一点也 OK。
//
// 日志：debug 级前/后各一条；info 级当 batch >= 50 时摘要（避免 binlog 高峰
// 期 debug 刷屏）。失败 warn + 数量 + 抽样首条 svc/table 给 oncall。
func (p *Publisher) PublishBatch(ctx context.Context, events []*Event) error {
	if len(events) == 0 {
		return nil
	}
	// 写入前：批量摘要日志（同 svc/table 计数让搜索"为什么搜不到"一目了然）
	tableCounts := make(map[string]int, 8)
	for _, e := range events {
		tableCounts[e.Service+":"+e.Table]++
	}
	p.logger.Debug("redis publish batch: begin",
		zap.Int("events", len(events)),
		zap.Any("by_table", tableCounts))

	start := time.Now()
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
	dur := time.Since(start)
	if p.OnPublish != nil {
		for _, e := range events {
			p.OnPublish(e.Service, e.Table, e.Op, err)
		}
	}
	if err != nil {
		// 抓首条事件 svc/table 给排查用
		var sample string
		if len(events) > 0 {
			sample = events[0].Service + ":" + events[0].Table
		}
		p.logger.Warn("redis publish batch: failed",
			zap.Int("events", len(events)),
			zap.String("sample", sample),
			zap.Duration("dur", dur),
			zap.Error(err))
		return err
	}
	// 写入成功：debug 级；批量大时升 info 让 ops 能在 prod 默认 info 级看到
	// 实时搜索 "为什么搜不到 / 搜得到" 的判定信号。
	if len(events) >= 50 {
		p.logger.Info("redis publish batch: ok (large batch)",
			zap.Int("events", len(events)),
			zap.Any("by_table", tableCounts),
			zap.Duration("dur", dur))
	} else {
		p.logger.Debug("redis publish batch: ok",
			zap.Int("events", len(events)),
			zap.Any("by_table", tableCounts),
			zap.Duration("dur", dur))
	}
	// PERF-15: 标记 batch 内所有 (svc, table). 内部 LoadOrStore 去重,
	// 真正 SADD Redis 的只有首次见到的 pair, 不会刷爆.
	if p.nonEmpty != nil {
		seen := make(map[string]bool, len(tableCounts))
		for _, e := range events {
			k := e.Service + "|" + e.Table
			if seen[k] {
				continue
			}
			seen[k] = true
			p.nonEmpty.Mark(ctx, p.r, e.Service, e.Table)
		}
	}
	return nil
}

// SavePosition 持久化某 service 的 binlog 位点（旧 API；单实例场景）。
//
// canal flush 周期调（每秒一次足够），不放进每条事件 pipeline，避免 fsync 风暴。
func (p *Publisher) SavePosition(ctx context.Context, service, file string, pos uint32, gtid string) error {
	return p.SaveChannelPosition(ctx, service, 0, file, pos, gtid)
}

// SaveChannelPosition 持久化某 service 第 idx 个 channel 的位点（多分片场景）。
//
// 每分片独立位点，重启时各 channel 从自己的 saved pos 起跑。
//
//	recon:cdc:pos:order-core:0  shard-0 的位点
//	recon:cdc:pos:order-core:1  shard-1
//	...
func (p *Publisher) SaveChannelPosition(ctx context.Context, service string, channelIdx int, file string, pos uint32, gtid string) error {
	key := fmt.Sprintf("recon:cdc:pos:%s:%d", service, channelIdx)
	return p.r.HSet(ctx, key, map[string]any{
		"file":  file,
		"pos":   pos,
		"gtid":  gtid,
		"saved": time.Now().UnixMilli(),
	}).Err()
}

// LoadPosition 旧 API（单实例场景，等价 LoadChannelPosition(_, _, 0, ...))。
func (p *Publisher) LoadPosition(ctx context.Context, service string) (file string, pos uint32, gtid string, ok bool) {
	return p.LoadChannelPosition(ctx, service, 0)
}

// LoadChannelPosition 拿某 service 第 idx 个 channel 的保存位点。
func (p *Publisher) LoadChannelPosition(ctx context.Context, service string, channelIdx int) (file string, pos uint32, gtid string, ok bool) {
	// 优先读新 key (带 idx)；老的单实例 fallback 兼容旧数据
	key := fmt.Sprintf("recon:cdc:pos:%s:%d", service, channelIdx)
	m, err := p.r.HGetAll(ctx, key).Result()
	if err != nil || len(m) == 0 {
		// fallback 兼容老 key
		if channelIdx == 0 {
			oldKey := "recon:cdc:pos:" + service
			m, _ = p.r.HGetAll(ctx, oldKey).Result()
		}
	}
	if len(m) == 0 {
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

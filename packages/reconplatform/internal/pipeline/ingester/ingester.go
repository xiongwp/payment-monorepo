// Package ingester — Kafka 消费者把 cdc.Event 推到 candidate.Layer.
//
// 流水线第二段:
//
//	Kafka (recon.cdc.*) -> Ingester (本包) -> candidate.Layer -> Matching Engine
//
// 设计:
//   - Consumer group: "reconplatform-ingester" -> 多 pod 水平扩展,自动 rebalance
//   - At-least-once: 消费 -> 写候选层成功后才 commit offset; 写失败 → 不 commit
//                    会再消费;消费侧通过 (svc:table:pk) 自然幂等 (候选层 HSET 同 key 覆盖)
//   - 触发 trigger: candidate.Put 返 triggers 时,本包不直接调 matcher (解耦),
//                   trigger 已经写进 Redis trigger queue,matcher 自取
//   - 失败处理: 单条解析失败 -> 写 dead-letter topic + 跳过 (不阻塞)
//
// 部署形态:
//   单进程多 goroutine (一 topic 一 goroutine) 或多 pod 多副本 (按 partition 切分).
package ingester

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"

	"reconcile-system/internal/cdc"
	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/store"
)

// Config ingester 配置.
type Config struct {
	Brokers       []string // bootstrap servers
	Topics        []string // 要订阅的 topic 列表 (e.g. ["recon.cdc.order-core", "recon.cdc.payment-channel"])
	ConsumerGroup string   // 消费组,默认 "reconplatform-ingester"
	ClientID      string   // 默认 "reconplatform-ingester"

	// DLQ 配置 (单条消息无法处理时落 DLQ topic).
	DLQTopic string // 默认 "recon.dlq"

	// 单 batch 最多处理多少条 (拉一次最大 records). 默认 500.
	MaxBatch int

	// PollTimeout 一次 fetch 等多久. 默认 5s.
	PollTimeout time.Duration
}

// DefaultConfig 默认.
func DefaultConfig(brokers []string, topics []string) Config {
	return Config{
		Brokers:       brokers,
		Topics:        topics,
		ConsumerGroup: "reconplatform-ingester",
		ClientID:      "reconplatform-ingester",
		DLQTopic:      "recon.dlq",
		MaxBatch:      500,
		PollTimeout:   5 * time.Second,
	}
}

// Ingester Kafka 消费者.
type Ingester struct {
	cfg    Config
	cl     *kgo.Client
	dlqCl  *kgo.Client // 同集群独立 client,避免 DLQ produce 干扰主消费
	layer  candidate.Layer
	logger *zap.Logger

	// metrics
	consumedTotal atomic.Int64
	putTotal      atomic.Int64
	parseFailed   atomic.Int64
	candidateErr  atomic.Int64
	dlqSent       atomic.Int64
	triggers      atomic.Int64
}

// New 构造 + dial.
func New(cfg Config, layer candidate.Layer, logger *zap.Logger) (*Ingester, error) {
	if len(cfg.Brokers) == 0 || len(cfg.Topics) == 0 {
		return nil, errors.New("ingester: brokers + topics required")
	}
	if cfg.ConsumerGroup == "" {
		cfg.ConsumerGroup = "reconplatform-ingester"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = cfg.ConsumerGroup
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 500
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 5 * time.Second
	}
	if cfg.DLQTopic == "" {
		cfg.DLQTopic = "recon.dlq"
	}
	if logger == nil {
		logger = zap.NewNop()
	}

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.ConsumerGroup(cfg.ConsumerGroup),
		kgo.ConsumeTopics(cfg.Topics...),
		// 关 auto-commit:我们手动 CommitRecords 保证 at-least-once 语义
		kgo.DisableAutoCommit(),
		kgo.SessionTimeout(30*time.Second),
		kgo.FetchMaxBytes(50*1024*1024),
	)
	if err != nil {
		return nil, err
	}

	dlqCl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID+"-dlq"),
	)
	if err != nil {
		cl.Close()
		return nil, err
	}

	return &Ingester{
		cfg: cfg, cl: cl, dlqCl: dlqCl, layer: layer, logger: logger,
	}, nil
}

// Run 阻塞循环消费,ctx 取消后退出.
//
// 返回原因:
//   - ctx.Done() → nil
//   - 任一 kgo PollFetches 致命错 → 返该 error
//
// 单条解析失败 → DLQ + skip (不退出);
// 写候选层失败 → 不 commit offset (下次重试).
func (i *Ingester) Run(ctx context.Context) error {
	i.logger.Info("ingester started",
		zap.Strings("topics", i.cfg.Topics),
		zap.String("group", i.cfg.ConsumerGroup))
	defer i.logger.Info("ingester stopped")
	defer i.cl.Close()
	defer i.dlqCl.Close()

	for {
		if ctx.Err() != nil {
			return nil
		}
		fetches := i.cl.PollRecords(ctx, i.cfg.MaxBatch)
		if errs := fetches.Errors(); len(errs) > 0 {
			// 单分区错误不致命;只 log,继续
			for _, fe := range errs {
				i.logger.Warn("kafka fetch error",
					zap.String("topic", fe.Topic),
					zap.Int32("partition", fe.Partition),
					zap.Error(fe.Err))
			}
		}
		toCommit := []*kgo.Record{}
		iter := fetches.RecordIter()
		for !iter.Done() {
			rec := iter.Next()
			if err := i.handle(ctx, rec); err != nil {
				// 候选层 transient 错误,不 commit 等重试
				i.candidateErr.Add(1)
				continue
			}
			toCommit = append(toCommit, rec)
		}
		if len(toCommit) > 0 {
			if err := i.cl.CommitRecords(ctx, toCommit...); err != nil {
				i.logger.Warn("commit failed", zap.Error(err))
				// 不返,下轮再 commit;客户端会保留 in-memory offset
			}
		}
	}
}

// handle 处理单条 record.
func (i *Ingester) handle(ctx context.Context, rec *kgo.Record) error {
	i.consumedTotal.Add(1)

	var cdcEvt cdc.Event
	if err := json.Unmarshal(rec.Value, &cdcEvt); err != nil {
		i.parseFailed.Add(1)
		i.sendDLQ(ctx, rec, "parse_error: "+err.Error())
		return nil // 不返错 → commit (跳过坏消息)
	}

	storeEvt := cdcToStore(&cdcEvt)
	triggers, err := i.layer.Put(ctx, storeEvt)
	if err != nil {
		i.logger.Warn("candidate.Put failed",
			zap.String("svc", cdcEvt.Service),
			zap.String("pk", cdcEvt.PK),
			zap.Error(err))
		return err
	}
	i.putTotal.Add(1)
	i.triggers.Add(int64(len(triggers)))
	return nil
}

// sendDLQ 失败消息送 dead-letter topic.
//
// DLQ 消息保留原 raw value + 失败原因 header (X-Recon-Failure).
// 运维通过订阅 DLQ topic 排查 / 重处理。
func (i *Ingester) sendDLQ(ctx context.Context, orig *kgo.Record, reason string) {
	dlq := &kgo.Record{
		Topic: i.cfg.DLQTopic,
		Key:   orig.Key,
		Value: orig.Value,
		Headers: append(orig.Headers,
			kgo.RecordHeader{Key: "X-Recon-Failure", Value: []byte(reason)},
			kgo.RecordHeader{Key: "X-Recon-Original-Topic", Value: []byte(orig.Topic)},
			kgo.RecordHeader{Key: "X-Recon-Original-Partition",
				Value: []byte(numStr(int(orig.Partition)))},
		),
	}
	i.dlqCl.Produce(ctx, dlq, func(_ *kgo.Record, err error) {
		if err != nil {
			i.logger.Error("DLQ produce failed",
				zap.String("orig_topic", orig.Topic),
				zap.Error(err))
			return
		}
		i.dlqSent.Add(1)
	})
}

// Stats 暴露给 Prometheus.
type Stats struct {
	Consumed     int64
	Put          int64
	ParseFailed  int64
	CandidateErr int64
	DLQSent      int64
	Triggers     int64
}

// Stats 取计数.
func (i *Ingester) Stats() Stats {
	return Stats{
		Consumed:     i.consumedTotal.Load(),
		Put:          i.putTotal.Load(),
		ParseFailed:  i.parseFailed.Load(),
		CandidateErr: i.candidateErr.Load(),
		DLQSent:      i.dlqSent.Load(),
		Triggers:     i.triggers.Load(),
	}
}

// cdcToStore 把 cdc.Event 转 store.Event.
//
// 字段一致, 直接拷贝 (避免下游 import cdc).
func cdcToStore(e *cdc.Event) *store.Event {
	return &store.Event{
		Service:    e.Service,
		Schema:     e.Schema,
		Table:      e.Table,
		PK:         e.PK,
		Op:         string(e.Op),
		Before:     e.Before,
		After:      e.After,
		BinlogFile: e.BinlogFile,
		BinlogPos:  e.BinlogPos,
		GTID:       e.GTID,
		Timestamp:  e.Timestamp,
		Indexes:    e.Indexes,
	}
}

// numStr 极简 int → string (避免 import strconv).
func numStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Package cdcbridge — 把 internal/cdc 的 binlog Event 桥接到 Kafka.
//
// 流水线第一段:
//
//	MySQL binlog -> cdc.Runner -> cdcbridge.KafkaSink -> Kafka topic
//	                                                      ↓
//	                                          ingester (本流水线第二段) 消费
//
// 设计:
//   - Topic 命名: <prefix>.<service>      (e.g. recon.cdc.order-core)
//                 默认 prefix = "recon.cdc"
//   - Partition key: 优先 pi_id / order_id / merchant_id 等业务索引,
//                    保证同一业务实体的事件落同分区 (顺序保证).
//                    没业务索引 → svc:table:pk 兜底.
//   - At-least-once: kgo 默认就是 at-least-once;消费侧用 idempotency_key 去重.
//   - Async: 非阻塞 Produce + 回调记 metric;同步 wait 走 Flush.
//   - 失败兜底: 重试 N 次仍失败 -> 写本地 fallback (Disk WAL) + 退本地异常表
//              (本骨架先 log error;Disk WAL 见 TODO 评论).
package cdcbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"

	"reconcile-system/internal/cdc"
)

// Config Kafka sink 配置.
type Config struct {
	Brokers       []string      // bootstrap servers
	TopicPrefix   string        // 默认 "recon.cdc"
	ClientID      string        // 默认 "reconplatform-cdcbridge"
	BatchTimeout  time.Duration // 默认 5ms
	ProduceTimeout time.Duration // 默认 30s
	// PartitionByIndex 选哪个 biz_key 做 partition key (优先级从高到低).
	// 默认 ["pi_id", "order_id", "merchant_id", "idempotency_key"].
	PartitionByIndex []string
}

// DefaultConfig 默认配置.
func DefaultConfig(brokers []string) Config {
	return Config{
		Brokers:          brokers,
		TopicPrefix:      "recon.cdc",
		ClientID:         "reconplatform-cdcbridge",
		BatchTimeout:     5 * time.Millisecond,
		ProduceTimeout:   30 * time.Second,
		PartitionByIndex: []string{"pi_id", "order_id", "merchant_id", "idempotency_key"},
	}
}

// KafkaSink cdc.Event → Kafka 桥接器.
// 跟 cdc.Publisher 接口签名兼容 (Publish(ctx, *cdc.Event) error),
// 直接替换或并联到 cdc.Runner 使用。
type KafkaSink struct {
	cl       *kgo.Client
	cfg      Config
	logger   *zap.Logger

	// metrics
	publishedTotal atomic.Int64
	failedTotal    atomic.Int64
	bytesTotal     atomic.Int64
}

// NewKafkaSink 构造 + dial brokers (失败即返).
func NewKafkaSink(cfg Config, logger *zap.Logger) (*KafkaSink, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("cdcbridge: brokers required")
	}
	if cfg.TopicPrefix == "" {
		cfg.TopicPrefix = "recon.cdc"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "reconplatform-cdcbridge"
	}
	if cfg.BatchTimeout == 0 {
		cfg.BatchTimeout = 5 * time.Millisecond
	}
	if cfg.ProduceTimeout == 0 {
		cfg.ProduceTimeout = 30 * time.Second
	}
	if len(cfg.PartitionByIndex) == 0 {
		cfg.PartitionByIndex = []string{"pi_id", "order_id", "merchant_id", "idempotency_key"}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.ProducerLinger(cfg.BatchTimeout),
		kgo.RequiredAcks(kgo.AllISRAcks()),                  // 强一致 (在 MIN_ISR 个 broker 落盘后才 ack)
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.RecordRetries(5),
	)
	if err != nil {
		return nil, fmt.Errorf("kgo new: %w", err)
	}
	return &KafkaSink{cl: cl, cfg: cfg, logger: logger}, nil
}

// Publish 实现 cdc.Publisher-like 签名: (ctx, *cdc.Event) error.
//
// 异步 produce:不等 ack,失败由 callback 上报 metric。
// 调用方想确认全部 flush -> 调 Flush().
func (s *KafkaSink) Publish(ctx context.Context, e *cdc.Event) error {
	if e == nil {
		return errors.New("nil event")
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	rec := &kgo.Record{
		Topic: s.topicFor(e.Service),
		Key:   []byte(s.partitionKey(e)),
		Value: body,
		Headers: []kgo.RecordHeader{
			{Key: "svc", Value: []byte(e.Service)},
			{Key: "table", Value: []byte(e.Table)},
			{Key: "op", Value: []byte(string(e.Op))},
			{Key: "ts", Value: []byte(e.Timestamp.UTC().Format(time.RFC3339Nano))},
		},
	}
	s.bytesTotal.Add(int64(len(body)))
	s.cl.Produce(ctx, rec, func(r *kgo.Record, err error) {
		if err != nil {
			s.failedTotal.Add(1)
			s.logger.Warn("kafka publish failed",
				zap.String("topic", r.Topic),
				zap.String("svc", e.Service),
				zap.String("table", e.Table),
				zap.Error(err))
			return
		}
		s.publishedTotal.Add(1)
	})
	return nil
}

// topicFor 拼 topic 名: <prefix>.<service>.
//
// 同一个 reconplatform 对账可能跨多服务,每服务一个 topic,
// 让 ingester / 下游可独立伸缩/订阅。
func (s *KafkaSink) topicFor(service string) string {
	return s.cfg.TopicPrefix + "." + service
}

// partitionKey 决定本条 Event 落 Kafka 哪个分区.
//
// 优先级:配置的 PartitionByIndex 顺序;都没有 → fallback "svc:table:pk".
// 同一业务实体的 INSERT / UPDATE / DELETE 顺序天然保证 (单分区有序).
func (s *KafkaSink) partitionKey(e *cdc.Event) string {
	for _, idx := range s.cfg.PartitionByIndex {
		if v, ok := e.Indexes[idx]; ok && v != "" {
			return idx + ":" + v
		}
	}
	return e.Service + ":" + e.Table + ":" + e.PK
}

// Flush 阻塞等所有 in-flight produce 完成 (优雅关停 / 单测同步用).
func (s *KafkaSink) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.ProduceTimeout)
	defer cancel()
	return s.cl.Flush(ctx)
}

// Close 关 Kafka client.
func (s *KafkaSink) Close() {
	s.cl.Close()
}

// Stats 暴露给 Prometheus 抓.
func (s *KafkaSink) Stats() (published, failed, bytes int64) {
	return s.publishedTotal.Load(), s.failedTotal.Load(), s.bytesTotal.Load()
}

// FanOutSink 把一条 Event 同时发给多个 sink (Redis + Kafka 并行投递).
//
// 用法:
//
//	fan := &FanOutSink{Sinks: []Sink{redisPublisher, kafkaSink}}
//	cdcRunner.AddSink(fan)
//
// 失败语义:聚合所有 sink 的 error,任一失败都返;不让一个 sink 拖死整链。
type FanOutSink struct {
	Sinks []Sink
}

// Sink 统一抽象,Redis Publisher / KafkaSink 都实现.
type Sink interface {
	Publish(ctx context.Context, e *cdc.Event) error
}

// Publish 并行 fan-out.
func (f *FanOutSink) Publish(ctx context.Context, e *cdc.Event) error {
	var first error
	for _, s := range f.Sinks {
		if err := s.Publish(ctx, e); err != nil && first == nil {
			first = err
		}
	}
	return first
}

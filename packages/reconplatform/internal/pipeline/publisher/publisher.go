// Package publisher — 把 MatchResult 序列化推 Kafka result topic.
//
// 流水线最后一段:
//
//	Matcher -> publisher (本包) -> Kafka recon.results -> [alerting / archive / dashboard]
//
// 设计:
//   - Topic: 默认 "recon.results";按 verdict 分 topic 是反模式 (订阅复杂),
//     用 message header 区分让消费者过滤.
//   - Partition key: TriggerKey ("biz_key:value") -> 同实体顺序稳定,
//     便于下游 archive 按时间维度聚合.
//   - Headers: verdict / rule_name / trigger_key / worker_id 都打到 header,
//     消费者可只读 header 决定是否要解 body (减少反序列化开销).
//   - At-least-once: kgo 默认 retry 5 次;Flush 用于优雅退出.
//   - 可插拔: 实现 matcher.Publisher 接口,可被替换为 NoopPublisher (测试) /
//     MultiSink (Kafka + ClickHouse 直写并行).
package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"

	"reconcile-system/internal/pipeline/matcher"
)

// Config Kafka publisher 配置.
type Config struct {
	Brokers      []string
	Topic        string // 默认 "recon.results"
	ClientID     string // 默认 "reconplatform-result-pub"
	BatchTimeout time.Duration
	FlushTimeout time.Duration
}

// DefaultConfig 默认.
func DefaultConfig(brokers []string) Config {
	return Config{
		Brokers:      brokers,
		Topic:        "recon.results",
		ClientID:     "reconplatform-result-pub",
		BatchTimeout: 5 * time.Millisecond,
		FlushTimeout: 30 * time.Second,
	}
}

// KafkaPublisher 实现 matcher.Publisher.
type KafkaPublisher struct {
	cl     *kgo.Client
	cfg    Config
	logger *zap.Logger

	published atomic.Int64
	failed    atomic.Int64
	bytes     atomic.Int64
}

// NewKafkaPublisher 构造 + dial.
func NewKafkaPublisher(cfg Config, logger *zap.Logger) (*KafkaPublisher, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("publisher: brokers required")
	}
	if cfg.Topic == "" {
		cfg.Topic = "recon.results"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "reconplatform-result-pub"
	}
	if cfg.BatchTimeout == 0 {
		cfg.BatchTimeout = 5 * time.Millisecond
	}
	if cfg.FlushTimeout == 0 {
		cfg.FlushTimeout = 30 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID),
		kgo.ProducerLinger(cfg.BatchTimeout),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.SnappyCompression()),
		kgo.RecordRetries(5),
	)
	if err != nil {
		return nil, err
	}
	return &KafkaPublisher{cl: cl, cfg: cfg, logger: logger}, nil
}

// Publish 实现 matcher.Publisher.
func (p *KafkaPublisher) Publish(ctx context.Context, r matcher.MatchResult) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	rec := &kgo.Record{
		Topic: p.cfg.Topic,
		Key:   []byte(r.TriggerKey.String()),
		Value: body,
		Headers: []kgo.RecordHeader{
			{Key: "verdict", Value: []byte(string(r.Verdict))},
			{Key: "rule", Value: []byte(r.RuleName)},
			{Key: "trigger", Value: []byte(r.TriggerKey.String())},
			{Key: "worker", Value: []byte(r.WorkerID)},
			{Key: "ts", Value: []byte(r.MatchedAt.UTC().Format(time.RFC3339Nano))},
		},
	}
	p.bytes.Add(int64(len(body)))
	p.cl.Produce(ctx, rec, func(rec *kgo.Record, err error) {
		if err != nil {
			p.failed.Add(1)
			p.logger.Warn("result publish failed",
				zap.String("topic", rec.Topic),
				zap.String("trigger", r.TriggerKey.String()),
				zap.String("rule", r.RuleName),
				zap.Error(err))
			return
		}
		p.published.Add(1)
	})
	return nil
}

// Flush 等所有 in-flight 完成 (优雅退出用).
func (p *KafkaPublisher) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.FlushTimeout)
	defer cancel()
	return p.cl.Flush(ctx)
}

// Close 关 client.
func (p *KafkaPublisher) Close() { p.cl.Close() }

// Stats 监控用.
type Stats struct{ Published, Failed, Bytes int64 }

// Stats 取计数.
func (p *KafkaPublisher) Stats() Stats {
	return Stats{Published: p.published.Load(), Failed: p.failed.Load(), Bytes: p.bytes.Load()}
}

// ─── NoopPublisher (测试用) ───────────────────────────────────

// NoopPublisher 丢弃结果 (单测 / dev mock).
type NoopPublisher struct {
	Count atomic.Int64
}

// Publish impl.
func (n *NoopPublisher) Publish(_ context.Context, _ matcher.MatchResult) error {
	n.Count.Add(1)
	return nil
}

// ─── MultiSink: 多 sink 并行 (Kafka + 直写 ClickHouse 等) ───────

// MultiSink 把一条 MatchResult 推到多个 publisher.
// 失败返第一个 error,不阻塞剩余 sink.
type MultiSink struct {
	Sinks []matcher.Publisher
}

// Publish impl.
func (m *MultiSink) Publish(ctx context.Context, r matcher.MatchResult) error {
	var first error
	for _, s := range m.Sinks {
		if err := s.Publish(ctx, r); err != nil && first == nil {
			first = err
		}
	}
	return first
}

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
	"fmt"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
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

	// DLQ (REL-1): 主 topic produce 失败 / 熔断时,把 MatchResult 写 DLQ 让运维捞回.
	// 默认 "recon.results.dlq".空 = 禁用 DLQ (失败直接丢).
	DLQTopic string

	// BreakerThreshold (REL-1): 连续失败多少次开闸,默认 5.
	BreakerThreshold int64
	// BreakerCooldown (REL-1): Open 状态多久后允许 HalfOpen 试探,默认 30s.
	BreakerCooldown time.Duration
}

// DefaultConfig 默认.
func DefaultConfig(brokers []string) Config {
	return Config{
		Brokers:          brokers,
		Topic:            "recon.results",
		ClientID:         "reconplatform-result-pub",
		BatchTimeout:     5 * time.Millisecond,
		FlushTimeout:     30 * time.Second,
		DLQTopic:         "recon.results.dlq",
		BreakerThreshold: 5,
		BreakerCooldown:  30 * time.Second,
	}
}

// KafkaPublisher 实现 matcher.Publisher.
type KafkaPublisher struct {
	cl     *kgo.Client
	dlqCl  *kgo.Client // REL-1: 独立 client,主 topic Open 时往这里塞
	cfg    Config
	logger *zap.Logger

	// REL-1: circuit breaker, 跟 produce 回调联动
	breaker *circuitBreaker

	// UX-2: shadow mode 走 Redis stream 不走主 Kafka topic.
	// 可选 — 没挂 Redis 时 shadow result 直接丢弃 + warn 一次.
	shadowRdb       redis.UniversalClient
	shadowStreamKey string
	shadowMaxLen    int64

	published   atomic.Int64
	failed      atomic.Int64
	bytes       atomic.Int64
	dlqSent     atomic.Int64
	shadowSent  atomic.Int64
	breakerOpen atomic.Int64 // 状态变 Open 次数
}

// WithShadowSink (UX-2) 让 shadow 模式的 MatchResult 走 Redis stream 而非主 Kafka topic.
//
// streamKey 默认 "recon:shadow:diff" (admin 端有专用查看页);
// maxLen 默认 10000 (XADD ~ MAXLEN approximated).
//
// 没挂 → shadow result 直接丢弃 + 首次 warn.
func (p *KafkaPublisher) WithShadowSink(rdb redis.UniversalClient, streamKey string, maxLen int64) *KafkaPublisher {
	if streamKey == "" {
		streamKey = "recon:shadow:diff"
	}
	if maxLen <= 0 {
		maxLen = 10000
	}
	p.shadowRdb = rdb
	p.shadowStreamKey = streamKey
	p.shadowMaxLen = maxLen
	return p
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
	if cfg.DLQTopic == "" {
		cfg.DLQTopic = "recon.results.dlq"
	}
	if cfg.BreakerThreshold <= 0 {
		cfg.BreakerThreshold = 5
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = 30 * time.Second
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
	// REL-1: DLQ client 独立, 避免与主 client 共用 producer 队列阻塞.
	dlqCl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID(cfg.ClientID+"-dlq"),
		kgo.ProducerLinger(20*time.Millisecond), // DLQ 可缓一点
		kgo.RecordRetries(3),
	)
	if err != nil {
		cl.Close()
		return nil, err
	}
	p := &KafkaPublisher{cl: cl, dlqCl: dlqCl, cfg: cfg, logger: logger}
	p.breaker = newCircuitBreaker(cfg.BreakerThreshold, cfg.BreakerCooldown)
	p.breaker.OnTransition = func(from, to string) {
		logger.Warn("publisher breaker transition",
			zap.String("from", from), zap.String("to", to))
		if to == "open" {
			p.breakerOpen.Add(1)
		}
	}
	return p, nil
}

// Publish 实现 matcher.Publisher.
//
// REL-1 路径:
//   - breaker Closed/HalfOpen → 走主 topic; produce 回调成功 OnSuccess(), 失败 OnFailure() 并落 DLQ.
//   - breaker Open            → 直接落 DLQ + 返 nil (matcher 不重试,避免堆积).
//
// UX-2 路径:
//   - r.Shadow=true → 跳过主 Kafka topic, 写 Redis stream recon:shadow:diff
//     供运营 UI 灰度对比 (24h 看新规则的命中/误报, 再决定切 live).
//
// DLQ 写入失败仅记 metric, 不冒泡 (匹配结果丢一条 < 流水线雪崩).
func (p *KafkaPublisher) Publish(ctx context.Context, r matcher.MatchResult) error {
	body, err := json.Marshal(r)
	if err != nil {
		return err
	}
	headers := []kgo.RecordHeader{
		{Key: "verdict", Value: []byte(string(r.Verdict))},
		{Key: "rule", Value: []byte(r.RuleName)},
		{Key: "trigger", Value: []byte(r.TriggerKey.String())},
		{Key: "worker", Value: []byte(r.WorkerID)},
		{Key: "ts", Value: []byte(r.MatchedAt.UTC().Format(time.RFC3339Nano))},
	}
	p.bytes.Add(int64(len(body)))

	// FEAT-2 + FEAT-3: 落 diff 计数到 Redis,给告警 / 趋势图用.
	// 仅 mismatched / orphan / error 算 "diff", matched / pending 不算.
	if p.shadowRdb != nil { // 借 shadow Redis client (admin 注入时一并提供)
		ruleID := r.RuleName
		v := string(r.Verdict)
		if v != "matched" && v != "pending" && v != "" && ruleID != "" {
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 1*time.Second)
				defer cancel()
				now := time.Now()
				ns := now.Unix()
				pipe := p.shadowRdb.Pipeline()
				// FEAT-2: 告警 ZSET (秒粒度 / 滑动窗口)
				zkey := "recon:alert:diffs:" + ruleID
				pipe.ZAdd(bg, zkey, redis.Z{
					Score:  float64(ns),
					Member: fmt.Sprintf("%d:%s:%s", ns, v, r.TriggerKey.String()),
				})
				pipe.ZRemRangeByScore(bg, zkey, "0", fmt.Sprintf("%d", ns-3600))
				pipe.Expire(bg, zkey, 2*time.Hour)
				// FEAT-3: 趋势计数 (HASH 小时粒度, EXPIRE 26h 保留 24h)
				hourKey := "recon:trend:" + ruleID
				hourField := now.UTC().Format("2006010215") // YYYYMMDDHH
				pipe.HIncrBy(bg, hourKey, hourField, 1)
				pipe.Expire(bg, hourKey, 26*time.Hour)
				_, _ = pipe.Exec(bg)
			}()
		}
	}

	// UX-2: shadow 走 Redis stream, 不走 Kafka.
	if r.Shadow {
		p.sendShadow(ctx, body, r)
		return nil
	}

	// REL-1: breaker 早判断, fail-fast 时直接走 DLQ
	if !p.breaker.Allow() {
		p.sendDLQ(ctx, body, headers, r, "circuit_open")
		return nil
	}

	rec := &kgo.Record{
		Topic:   p.cfg.Topic,
		Key:     []byte(r.TriggerKey.String()),
		Value:   body,
		Headers: headers,
	}
	p.cl.Produce(ctx, rec, func(rec *kgo.Record, perr error) {
		if perr != nil {
			p.failed.Add(1)
			p.breaker.OnFailure()
			p.logger.Warn("result publish failed → DLQ",
				zap.String("topic", rec.Topic),
				zap.String("trigger", r.TriggerKey.String()),
				zap.String("rule", r.RuleName),
				zap.String("breaker", p.breaker.State()),
				zap.Error(perr))
			// 失败的不走原主路径, 走 DLQ 保留待重处理
			p.sendDLQ(context.Background(), body, headers, r, "produce_error:"+perr.Error())
			return
		}
		p.published.Add(1)
		p.breaker.OnSuccess()
	})
	return nil
}

// sendDLQ 把 MatchResult 推 DLQ topic.
//
// reason header 标注分流原因: circuit_open / produce_error:<msg>.
// 失败仅记 metric (DLQ 也挂了 = Kafka 整体故障, 流水线降级运行).
func (p *KafkaPublisher) sendDLQ(ctx context.Context, body []byte, headers []kgo.RecordHeader, r matcher.MatchResult, reason string) {
	if p.cfg.DLQTopic == "" || p.dlqCl == nil {
		return
	}
	dlqHeaders := append(headers,
		kgo.RecordHeader{Key: "dlq-reason", Value: []byte(reason)},
		kgo.RecordHeader{Key: "dlq-original-topic", Value: []byte(p.cfg.Topic)},
	)
	rec := &kgo.Record{
		Topic:   p.cfg.DLQTopic,
		Key:     []byte(r.TriggerKey.String()),
		Value:   body,
		Headers: dlqHeaders,
	}
	p.dlqCl.Produce(ctx, rec, func(_ *kgo.Record, err error) {
		if err != nil {
			p.logger.Error("DLQ produce failed",
				zap.String("trigger", r.TriggerKey.String()),
				zap.String("reason", reason),
				zap.Error(err))
			return
		}
		p.dlqSent.Add(1)
	})
}

// BreakerState 暴露给 admin /healthz.
func (p *KafkaPublisher) BreakerState() string { return p.breaker.State() }

// sendShadow (UX-2) 把 shadow MatchResult 写 Redis stream.
//
// 字段格式:
//   rule / trigger / verdict / matched_at / detail_json
// 没挂 Redis → 首次 warn, 后续静默丢弃 (shadow 是诊断, 不阻塞).
func (p *KafkaPublisher) sendShadow(ctx context.Context, body []byte, r matcher.MatchResult) {
	if p.shadowRdb == nil {
		if p.shadowSent.Load() == 0 {
			p.logger.Warn("shadow result discarded: no shadow sink configured",
				zap.String("rule", r.RuleName))
		}
		p.shadowSent.Add(1)
		return
	}
	err := p.shadowRdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.shadowStreamKey,
		MaxLen: p.shadowMaxLen,
		Approx: true,
		Values: map[string]interface{}{
			"rule":       r.RuleName,
			"trigger":    r.TriggerKey.String(),
			"verdict":    string(r.Verdict),
			"matched_at": r.MatchedAt.UTC().Format(time.RFC3339Nano),
			"body":       string(body),
		},
	}).Err()
	if err != nil {
		p.logger.Warn("shadow xadd failed",
			zap.String("rule", r.RuleName),
			zap.Error(err))
		return
	}
	p.shadowSent.Add(1)
}

// Flush 等所有 in-flight 完成 (优雅退出用).
func (p *KafkaPublisher) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.FlushTimeout)
	defer cancel()
	// 主 + DLQ 都 flush, 退出前不丢消息.
	mainErr := p.cl.Flush(ctx)
	if p.dlqCl != nil {
		_ = p.dlqCl.Flush(ctx)
	}
	return mainErr
}

// Close 关 client.
func (p *KafkaPublisher) Close() {
	p.cl.Close()
	if p.dlqCl != nil {
		p.dlqCl.Close()
	}
}

// Stats 监控用.
type Stats struct {
	Published   int64  `json:"published"`
	Failed      int64  `json:"failed"`
	Bytes       int64  `json:"bytes"`
	DLQSent     int64  `json:"dlq_sent"`
	BreakerOpen int64  `json:"breaker_open_count"`
	BreakerState string `json:"breaker_state"`
}

// Stats 取计数.
func (p *KafkaPublisher) Stats() Stats {
	return Stats{
		Published:    p.published.Load(),
		Failed:       p.failed.Load(),
		Bytes:        p.bytes.Load(),
		DLQSent:      p.dlqSent.Load(),
		BreakerOpen:  p.breakerOpen.Load(),
		BreakerState: p.breaker.State(),
	}
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

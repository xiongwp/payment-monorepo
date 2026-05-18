// audit_kafka.go — SP-AC-7 PROD3: 把资金审计推 Kafka 独立 topic.
//
// 设计:
//   - 跟业务 log 隔离, 审计专属 topic (e.g. "split-payment.audit") 单独 ACL + 独立留存期
//   - ProduceSync 等 ACK, 保证 audit 真落 Kafka 才返回 (failed → 调用方知道)
//   - 失败后 fallback 到 ChainAuditSink 里其它 sink (e.g. ZapAuditSink)
//
// Topic key: business_no (charge_id), 让同一业务的 audit 落同 partition, 顺序读.
package grpcsvc

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

// KafkaAuditSink — 把 AuditEvent 推 Kafka audit topic.
type KafkaAuditSink struct {
	Topic string // 默认 "split-payment.audit"
	cl    *kgo.Client
	log   *zap.Logger
}

// NewKafkaAuditSink — brokers 空 → 返 nil 让调用方知道 audit Kafka 没启 (退化为只本地 zap).
func NewKafkaAuditSink(brokers []string, topic string, log *zap.Logger) (*KafkaAuditSink, error) {
	if len(brokers) == 0 {
		return nil, errors.New("no brokers")
	}
	if topic == "" {
		topic = "split-payment.audit"
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.RecordRetries(3),
		kgo.RequestTimeoutOverhead(5*time.Second),
	)
	if err != nil {
		return nil, err
	}
	return &KafkaAuditSink{Topic: topic, cl: cl, log: log}, nil
}

// Write 同步推到 Kafka. 失败返 err, ChainAuditSink 会跳过它继续下个 sink.
func (k *KafkaAuditSink) Write(ctx context.Context, ev AuditEvent) error {
	if k == nil || k.cl == nil {
		return nil
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	rec := &kgo.Record{
		Topic: k.Topic,
		Key:   []byte(ev.BusinessNo), // 同 business_no audit 同 partition 保证有序
		Value: body,
		Headers: []kgo.RecordHeader{
			{Key: "action", Value: []byte(ev.Action)},
			{Key: "trace_id", Value: []byte(ev.TraceID)},
		},
	}
	res := k.cl.ProduceSync(ctx, rec)
	if err := res.FirstErr(); err != nil {
		if k.log != nil {
			k.log.Warn("kafka audit produce failed (will fallback to other sinks)",
				zap.String("business_no", ev.BusinessNo), zap.Error(err))
		}
		return err
	}
	return nil
}

// Close.
func (k *KafkaAuditSink) Close() {
	if k != nil && k.cl != nil {
		k.cl.Close()
	}
}

// events.go — SP-8 Money Flow 状态机事件 + Kafka publisher.
//
// 事件类型 (标准化, Stripe-like):
//
//	transfer.created          translator 产出, 还没 post 到 accounting
//	transfer.posted           accounting 成功
//	transfer.failed           capability gate / accounting 拒绝
//	transfer.reversed         reverse 全部金额完成
//	transfer.partially_reversed  reverse 部分
//	application_fee.created
//	application_fee.collected accounting 成功记 fee
//	application_fee.refunded
//	application_fee.partially_refunded
//	payout.created
//	payout.in_transit         银行通道已发
//	payout.paid               银行回传成功
//	payout.failed
//	payout.canceled
//	reversal.created
//	reversal.succeeded
//	reversal.failed
//
// Kafka topic: recon.moneyflow.events (默认), partition key = transfer_group / charge_id / account
// 让同实体的事件落同分区, 下游消费有序.
//
// 下游消费者:
//   - merchant-webhook 服务: fan-out 给商户 HTTP webhook
//   - data-warehouse: 落 ClickHouse 给 BI
//   - audit: 不可改的事件溯源

package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"
)

// EventType 常量 — 跟 Stripe 命名风格对齐.
const (
	EventTransferCreated           = "transfer.created"
	EventTransferPosted            = "transfer.posted"
	EventTransferFailed            = "transfer.failed"
	EventTransferReversed          = "transfer.reversed"
	EventTransferPartiallyReversed = "transfer.partially_reversed"

	EventAppFeeCreated           = "application_fee.created"
	EventAppFeeCollected         = "application_fee.collected"
	EventAppFeeRefunded          = "application_fee.refunded"
	EventAppFeePartiallyRefunded = "application_fee.partially_refunded"

	EventPayoutCreated   = "payout.created"
	EventPayoutInTransit = "payout.in_transit"
	EventPayoutPaid      = "payout.paid"
	EventPayoutFailed    = "payout.failed"
	EventPayoutCanceled  = "payout.canceled"

	EventReversalCreated   = "reversal.created"
	EventReversalSucceeded = "reversal.succeeded"
	EventReversalFailed    = "reversal.failed"
)

// Event Money Flow 标准化事件 envelope.
//
// 跟 Stripe Event 对象同形态:
//
//	{
//	  "id": "evt_xxx",
//	  "type": "transfer.posted",
//	  "data": { "object": <Transfer/Payout/...> },
//	  "created": <unix>,
//	  "livemode": true
//	}
type Event struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Data     EventData       `json:"data"`
	Created  int64           `json:"created"`  // unix sec
	LiveMode bool            `json:"livemode"`
}

// EventData 跟 Stripe 一致, 用 { "object": ... } 包装一层.
type EventData struct {
	Object any `json:"object"`
}

// NoopEventPublisher 不发, 给 dev / 单测.
type NoopEventPublisher struct{}

// Publish impl.
func (n NoopEventPublisher) Publish(_ context.Context, _ string, _ any) error { return nil }

// ─── Kafka publisher ──────────────────────────────────────────────────

// KafkaEventConfig.
type KafkaEventConfig struct {
	Brokers      []string
	Topic        string        // 默认 "recon.moneyflow.events"
	ClientID     string        // 默认 "split-payment-events"
	BatchTimeout time.Duration // 默认 5ms
	LiveMode     bool          // env != dev → true
}

// DefaultKafkaEventConfig.
func DefaultKafkaEventConfig(brokers []string) KafkaEventConfig {
	return KafkaEventConfig{
		Brokers:      brokers,
		Topic:        "recon.moneyflow.events",
		ClientID:     "split-payment-events",
		BatchTimeout: 5 * time.Millisecond,
		LiveMode:     false,
	}
}

// KafkaEventPublisher 把 Money Flow 事件推 Kafka.
//
// Partition key 按对象类型选择:
//   - Transfer / Reversal     → transfer_group (同 group 落同分区, 退款时按序)
//   - ApplicationFee          → charge (同 charge 落同分区)
//   - Payout                  → account (同账户 payout 落同分区)
//   - 未知                    → event.id (轮询)
//
// 异步 Produce (失败仅 metric + log), 退款 / reversal 这种关键路径建议主流程也持
// 久化到 DB, Kafka 只做 fan-out 给 webhook / 数仓.
type KafkaEventPublisher struct {
	cl       *kgo.Client
	cfg      KafkaEventConfig
	logger   *zap.Logger
	produced atomic.Int64
	failed   atomic.Int64
}

// NewKafkaEventPublisher 构造 + dial.
func NewKafkaEventPublisher(cfg KafkaEventConfig, logger *zap.Logger) (*KafkaEventPublisher, error) {
	if len(cfg.Brokers) == 0 {
		return nil, errors.New("kafka event publisher: brokers required")
	}
	if cfg.Topic == "" {
		cfg.Topic = "recon.moneyflow.events"
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "split-payment-events"
	}
	if cfg.BatchTimeout <= 0 {
		cfg.BatchTimeout = 5 * time.Millisecond
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
		return nil, fmt.Errorf("kgo new: %w", err)
	}
	return &KafkaEventPublisher{cl: cl, cfg: cfg, logger: logger}, nil
}

// Publish 接口实现.
func (p *KafkaEventPublisher) Publish(ctx context.Context, eventType string, payload any) error {
	evt := Event{
		ID:       genEventID(),
		Type:     eventType,
		Data:     EventData{Object: payload},
		Created:  time.Now().Unix(),
		LiveMode: p.cfg.LiveMode,
	}
	body, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	rec := &kgo.Record{
		Topic:   p.cfg.Topic,
		Key:     []byte(p.partitionKey(payload, evt.ID)),
		Value:   body,
		Headers: []kgo.RecordHeader{{Key: "type", Value: []byte(eventType)}},
	}
	p.cl.Produce(ctx, rec, func(_ *kgo.Record, err error) {
		if err != nil {
			p.failed.Add(1)
			p.logger.Warn("event publish failed",
				zap.String("type", eventType), zap.Error(err))
			return
		}
		p.produced.Add(1)
	})
	return nil
}

// partitionKey 同一对象的事件落同分区, 保证消费有序.
func (p *KafkaEventPublisher) partitionKey(obj any, fallback string) string {
	switch x := obj.(type) {
	case domain.Transfer:
		if x.TransferGroup != "" {
			return x.TransferGroup
		}
		return x.ID
	case *domain.Transfer:
		if x.TransferGroup != "" {
			return x.TransferGroup
		}
		return x.ID
	case domain.Reversal:
		return x.Transfer // 关联 transfer.ID, 跟 transfer 同分区
	case *domain.Reversal:
		return x.Transfer
	case domain.ApplicationFee:
		return x.Charge
	case *domain.ApplicationFee:
		return x.Charge
	case domain.Payout:
		return x.Account
	case *domain.Payout:
		return x.Account
	}
	return fallback
}

// Close 关 client.
func (p *KafkaEventPublisher) Close() { p.cl.Close() }

// SendRaw — SP-AC-7 L5: 同步发送已序列化好的 outbox payload (跳过 wrap Event 结构,
// 因为 outbox 表里存的就是 marshal 完的 Event JSON).
//
// 这个方法是 EventOutboxWorker → Kafka 的入口, ProduceSync 等 ACK 后才返回, 给 worker
// 准确的 "成功 / 失败" 信号. 失败 → worker 走重试逻辑.
//
// 跟 Publish 区别: Publish 是 fire-and-forget (业务路径), SendRaw 是同步 (worker 路径).
func (p *KafkaEventPublisher) SendRaw(ctx context.Context, eventType string, payload []byte) error {
	rec := &kgo.Record{
		Topic:   p.cfg.Topic,
		Value:   payload,
		Headers: []kgo.RecordHeader{{Key: "type", Value: []byte(eventType)}},
	}
	res := p.cl.ProduceSync(ctx, rec)
	if err := res.FirstErr(); err != nil {
		p.failed.Add(1)
		return fmt.Errorf("ProduceSync: %w", err)
	}
	p.produced.Add(1)
	return nil
}

// Flush 阻塞等所有 in-flight 完成.
func (p *KafkaEventPublisher) Flush(ctx context.Context) error {
	return p.cl.Flush(ctx)
}

// Stats 监控.
type EventPublisherStats struct {
	Produced int64
	Failed   int64
}

// Stats.
func (p *KafkaEventPublisher) Stats() EventPublisherStats {
	return EventPublisherStats{Produced: p.produced.Load(), Failed: p.failed.Load()}
}

// genEventID evt_<unix_nano hex>.
func genEventID() string {
	return fmt.Sprintf("evt_%016x", time.Now().UnixNano())
}

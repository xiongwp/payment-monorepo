// event_outbox_publisher.go — SP-AC-7 L5: outbox-backed EventPublisher.
//
// 实现 workflow.EventPublisher 接口, 但 Publish 不直接发 Kafka, 而是写 outbox 表.
// 配合 EventOutboxWorker 把 outbox 行 drain 到真实 Kafka producer.
//
// 这样保证: 业务 tx 提交 → 事件至少一次送达 (outbox 行存在 ↔ 事件最终会到 Kafka).
//
// 跟原 KafkaEventPublisher 区别:
//   - KafkaEventPublisher: 直发 Kafka, 失败仅 log warn → fire-and-forget, 可能漏接
//   - OutboxEventPublisher: 先落表, worker 异步发, 失败重试 → at-least-once
package workflow

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"
)

// EventOutboxEnqueuer worker 看到的最小接口 (避免循环 import repo).
type EventOutboxEnqueuer interface {
	Enqueue(ctx context.Context, eventType string, payloadJSON []byte) error
}

// OutboxEventPublisher 实现 EventPublisher 接口, Publish 写 outbox 而不是 Kafka.
type OutboxEventPublisher struct {
	Outbox EventOutboxEnqueuer
	Log    *zap.Logger
}

// Publish — 序列化 payload + Enqueue outbox.
//
// 失败处理: 序列化或入库失败 → log error 并返 err. 业务侧若调在 tx 内, tx rollback
// 会一起干掉 outbox 行, 资金跟事件保持一致.
func (p *OutboxEventPublisher) Publish(ctx context.Context, eventType string, payload any) error {
	if p.Outbox == nil {
		return nil // 退化为 noop
	}
	body, err := json.Marshal(payload)
	if err != nil {
		if p.Log != nil {
			p.Log.Error("OutboxEventPublisher: marshal failed",
				zap.String("event_type", eventType), zap.Error(err))
		}
		return err
	}
	if err := p.Outbox.Enqueue(ctx, eventType, body); err != nil {
		if p.Log != nil {
			p.Log.Error("OutboxEventPublisher: enqueue failed",
				zap.String("event_type", eventType), zap.Error(err))
		}
		return err
	}
	return nil
}

// ─── EventOutboxWorker — drain outbox → Kafka ────────────────────────────────

// EventOutboxClaimer worker 用接口.
type EventOutboxClaimer interface {
	Claim(ctx context.Context, limit int) ([]EventOutboxJob, error)
	MarkSent(ctx context.Context, id int64) error
	MarkDeadLetter(ctx context.Context, id int64, lastErr string) error
	UpdateError(ctx context.Context, id int64, lastErr string) error
}

// EventOutboxJob.
type EventOutboxJob struct {
	ID          int64
	EventType   string
	PayloadJSON []byte
	RetryCount  int
	MaxRetry    int
}

// EventSender 真实 Kafka producer 抽象 (let test mock).
type EventSender interface {
	Send(ctx context.Context, eventType string, payload []byte) error
}

// EventOutboxWorkerConfig.
type EventOutboxWorkerConfig struct {
	Interval  time.Duration
	BatchSize int
}

// DefaultEventOutboxConfig.
func DefaultEventOutboxConfig() EventOutboxWorkerConfig {
	return EventOutboxWorkerConfig{Interval: 5 * time.Second, BatchSize: 100}
}

// EventOutboxWorker.
type EventOutboxWorker struct {
	Cfg    EventOutboxWorkerConfig
	Outbox EventOutboxClaimer
	Sender EventSender
	Log    *zap.Logger
}

// Run 阻塞 ticker.
func (w *EventOutboxWorker) Run(ctx context.Context) {
	if w.Cfg.Interval <= 0 {
		w.Cfg.Interval = 5 * time.Second
	}
	if w.Cfg.BatchSize <= 0 {
		w.Cfg.BatchSize = 100
	}
	t := time.NewTicker(w.Cfg.Interval)
	defer t.Stop()
	if w.Log != nil {
		w.Log.Info("event outbox worker started",
			zap.Duration("interval", w.Cfg.Interval),
			zap.Int("batch", w.Cfg.BatchSize))
	}
	for {
		select {
		case <-ctx.Done():
			if w.Log != nil {
				w.Log.Info("event outbox worker stopped")
			}
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *EventOutboxWorker) tick(ctx context.Context) {
	if w.Outbox == nil || w.Sender == nil {
		return
	}
	jobs, err := w.Outbox.Claim(ctx, w.Cfg.BatchSize)
	if err != nil {
		if w.Log != nil {
			w.Log.Warn("event outbox: claim failed", zap.Error(err))
		}
		return
	}
	for _, j := range jobs {
		if err := w.Sender.Send(ctx, j.EventType, j.PayloadJSON); err != nil {
			if j.RetryCount+1 >= j.MaxRetry {
				if w.Log != nil {
					w.Log.Error("event outbox: dead-letter (max retry)",
						zap.Int64("id", j.ID),
						zap.String("event_type", j.EventType),
						zap.Error(err))
				}
				_ = w.Outbox.MarkDeadLetter(ctx, j.ID, err.Error())
				continue
			}
			if w.Log != nil {
				w.Log.Warn("event outbox: send failed, will retry",
					zap.Int64("id", j.ID),
					zap.String("event_type", j.EventType),
					zap.Int("retry", j.RetryCount),
					zap.Error(err))
			}
			_ = w.Outbox.UpdateError(ctx, j.ID, err.Error())
			continue
		}
		_ = w.Outbox.MarkSent(ctx, j.ID)
	}
}

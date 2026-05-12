// publisher.go — clearing-settlement 的 outbox publisher loop.
//
// 启动 (cmd/server/main.go):
//   pub := outboxhook.NewPublisher(db, kafkaWriter, log)
//   go pub.Loop(ctx)
//
// 单实例跑 (用 leader election 防多副本竞争; 这里简单版不带, 单 pod OK).

package outboxhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"go.uber.org/zap"
)

// KafkaSend 由调用方注入 (segmentio/kafka-go writer 包成 closure).
type KafkaSend func(ctx context.Context, topic string, key, value []byte, headers map[string]string) error

// Publisher 后台扫 outbox 表发 Kafka.
type Publisher struct {
	DB         *sql.DB
	Send       KafkaSend
	Log        *zap.Logger
	PollEvery  time.Duration
	BatchSize  int
	MaxRetries int
}

// NewPublisher 默认值
func NewPublisher(db *sql.DB, send KafkaSend, log *zap.Logger) *Publisher {
	return &Publisher{
		DB: db, Send: send, Log: log,
		PollEvery: 5 * time.Second, BatchSize: 100, MaxRetries: 10,
	}
}

// Loop 阻塞跑直到 ctx done.
func (p *Publisher) Loop(ctx context.Context) {
	t := time.NewTicker(p.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.tick(ctx); err != nil {
				p.Log.Warn("outbox tick", zap.Error(err))
			}
		}
	}
}

func (p *Publisher) tick(ctx context.Context) error {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT event_id, aggregate, aggregate_id, event_type, payload, topic, headers_json, retry_count
		  FROM tx_outbox
		 WHERE status = 'pending'
		 ORDER BY created_at
		 LIMIT ?`, p.BatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()

	type row struct {
		eventID, aggregate, aggregateID, eventType, topic string
		payload, headersJSON []byte
		retry int
	}
	var batch []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.eventID, &r.aggregate, &r.aggregateID, &r.eventType,
			&r.payload, &r.topic, &r.headersJSON, &r.retry); err != nil {
			return err
		}
		batch = append(batch, r)
	}
	for _, r := range batch {
		headers := map[string]string{"event_type": r.eventType, "aggregate": r.aggregate}
		if len(r.headersJSON) > 0 {
			_ = json.Unmarshal(r.headersJSON, &headers)
		}
		err := p.Send(ctx, r.topic, []byte(r.aggregateID), r.payload, headers)
		if err == nil {
			_, _ = p.DB.ExecContext(ctx, `
				UPDATE tx_outbox SET status='published', published_at=NOW(3)
				 WHERE event_id=?`, r.eventID)
			continue
		}
		// 失败
		newRetry := r.retry + 1
		if newRetry >= p.MaxRetries {
			_, _ = p.DB.ExecContext(ctx, `
				UPDATE tx_outbox SET status='failed', retry_count=?, last_error=?
				 WHERE event_id=?`, newRetry, err.Error(), r.eventID)
			p.Log.Error("outbox event DLQ", zap.String("event_id", r.eventID), zap.Error(err))
		} else {
			_, _ = p.DB.ExecContext(ctx, `
				UPDATE tx_outbox SET retry_count=?, last_error=? WHERE event_id=?`,
				newRetry, err.Error(), r.eventID)
		}
	}
	return nil
}

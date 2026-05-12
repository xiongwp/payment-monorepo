// consumer.go — outbox 消费方接口.
//
// 通常服务 A 用 outbox.Publisher 把事件发到 Kafka, 服务 B 通过 Kafka consumer 拿到事件.
// 但 consumer 端必须保证 *exactly-once* 处理 (上游可能重发同事件).
//
// 这里提供:
//   1. EventHandler 接口 — 业务侧实现
//   2. ConsumerLoop — 自动 dedup (按 event_id) + idempotent ack
//   3. processed_events 表 — 持久化已处理 event_id 防重 (跟业务行同事务写)
//
// 接入 (accounting-system 例):
//
//   h := &accountingHandler{db: db, log: log}
//   c := outbox.NewConsumer(h, kafkaReader, db, log)
//   go c.Loop(ctx)

package outbox

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// EventHandler 业务侧实现 — 单事件处理.
//
// 实现注意:
//   - 必须 idempotent (同 event 处理多次结果一致, 即使没 dedup 表)
//   - 业务 + processed_events 同事务写; 不要分别提交
//   - 返 nil 才认作成功; 返 err Consumer 重试
type EventHandler interface {
	Handle(ctx context.Context, tx *sql.Tx, evt Event) error
}

// Consumer 拉取 + 派发 + 记账.
type Consumer struct {
	Handler  EventHandler
	DB       *sql.DB
	Log      *zap.Logger
	Source   string // 用于 processed_events.source 列, 区分多 consumer
	Reader   MessageReader

	// 调度
	MaxRetries int

	// 指标
	mProcessed prometheus.Counter
	mDuplicate prometheus.Counter
	mFailed    prometheus.Counter
}

// MessageReader Kafka / NATS / SQS 拉取抽象.
type MessageReader interface {
	// ReadMessage 阻塞读一条; 返 nil err 表示成功读到; ctx done 返 ctx err.
	ReadMessage(ctx context.Context) (RawMessage, error)
	// CommitMessages 提交 offset (at-least-once 保证).
	CommitMessages(ctx context.Context, msgs ...RawMessage) error
}

type RawMessage struct {
	Topic     string
	Key       []byte
	Value     []byte // 原始 outbox.Event JSON
	Headers   map[string]string
	Timestamp time.Time
	// transport-specific (kafka partition / offset, sqs receipt handle …)
	Meta interface{}
}

// NewConsumer 构造
func NewConsumer(h EventHandler, r MessageReader, db *sql.DB, log *zap.Logger, source string) *Consumer {
	return &Consumer{
		Handler:    h,
		DB:         db,
		Log:        log,
		Source:     source,
		Reader:     r,
		MaxRetries: 10,
	}
}

// MustRegister 指标
func (c *Consumer) MustRegister(reg prometheus.Registerer, namespace string) {
	if namespace == "" {
		namespace = "outbox_consumer"
	}
	c.mProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "processed_total",
		Help: "successfully processed events",
	})
	c.mDuplicate = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "duplicate_total",
		Help: "duplicate events skipped",
	})
	c.mFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "failed_total",
		Help: "events that exhausted retries",
	})
	reg.MustRegister(c.mProcessed, c.mDuplicate, c.mFailed)
}

// Loop 阻塞跑.
func (c *Consumer) Loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msg, err := c.Reader.ReadMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			c.Log.Warn("outbox consumer read", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}
		if err := c.processOne(ctx, msg); err != nil {
			c.Log.Error("outbox consumer process", zap.Error(err))
			// 不 commit, 下次 retry
			continue
		}
		_ = c.Reader.CommitMessages(ctx, msg)
	}
}

// processOne 单事件 dedup + 业务处理同事务.
func (c *Consumer) processOne(ctx context.Context, msg RawMessage) error {
	// 解 Event (跟 publisher.go event.go 同结构)
	var evt Event
	if err := jsonUnmarshal(msg.Value, &evt); err != nil {
		// 解析失败 — 进 DLQ; 这里简化记 log
		c.Log.Error("outbox consumer bad event", zap.Error(err))
		return nil // 跳过 (不重试, 否则永远卡在 bad msg)
	}

	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// dedup check
	var dummy int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM outbox_processed WHERE event_id=? AND source=? LIMIT 1`,
		evt.EventID, c.Source).Scan(&dummy)
	if err == nil {
		if c.mDuplicate != nil {
			c.mDuplicate.Inc()
		}
		return tx.Commit() // 已处理 — commit 空 tx 让 reader 推进 offset
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// 业务 handler
	if err := c.Handler.Handle(ctx, tx, evt); err != nil {
		if c.mFailed != nil {
			c.mFailed.Inc()
		}
		return err
	}

	// 记账 — 跟业务同事务
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbox_processed (event_id, source, event_type, processed_at)
		VALUES (?, ?, ?, ?)`,
		evt.EventID, c.Source, evt.EventType, time.Now().UTC(),
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if c.mProcessed != nil {
		c.mProcessed.Inc()
	}
	return nil
}

// jsonUnmarshal 兜底, 真生产用 encoding/json
func jsonUnmarshal(data []byte, v *Event) error {
	return jsonDecode(data, v)
}

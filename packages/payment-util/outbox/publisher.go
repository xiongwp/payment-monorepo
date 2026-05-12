// publisher.go — outbox event publisher.
//
// 单实例跑 (用 row-level lock + UPDATE status WHERE event_id=... 防多副本竞争).
// 真生产建议:
//   - 用 leader election (etcd) 保证只一台机器有 publisher loop active
//   - 或多实例并发 SELECT ... FOR UPDATE SKIP LOCKED (mysql 8+) 横向扩
//
// 这里给基础实现 — 单 worker 轮询 + 批量发布 + 指数退避.

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// PublishFunc 由调用方注入 — 一般是 Kafka writer / NATS / SQS / HTTP webhook 等.
// 实现要 idempotent + 失败返 error (publisher 会重试 + backoff).
type PublishFunc func(ctx context.Context, evt Event) error

// Publisher 配置
type Publisher struct {
	DB           *sql.DB
	Publish      PublishFunc
	Log          *zap.Logger

	// 调度参数
	PollInterval  time.Duration // 默认 5s
	BatchSize     int           // 默认 100
	MaxRetries    int           // 默认 10
	FailedDLQ     bool          // true: retry 用尽 → status='failed', false: 永远重试

	// 指标
	mPublished    prometheus.Counter
	mFailed       prometheus.Counter
	mLag          prometheus.Gauge
}

// MustRegister 注册 prometheus 指标
func (p *Publisher) MustRegister(reg prometheus.Registerer, namespace string) {
	if namespace == "" {
		namespace = "outbox"
	}
	p.mPublished = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "events_published_total",
		Help: "events successfully published to downstream",
	})
	p.mFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace, Name: "events_failed_total",
		Help: "events that exhausted retries and moved to failed",
	})
	p.mLag = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "oldest_pending_seconds",
		Help: "age of oldest pending event in seconds",
	})
	reg.MustRegister(p.mPublished, p.mFailed, p.mLag)
}

// Loop 阻塞跑直到 ctx done.
func (p *Publisher) Loop(ctx context.Context) {
	if p.PollInterval == 0 {
		p.PollInterval = 5 * time.Second
	}
	if p.BatchSize == 0 {
		p.BatchSize = 100
	}
	if p.MaxRetries == 0 {
		p.MaxRetries = 10
	}
	tk := time.NewTicker(p.PollInterval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			if err := p.tick(ctx); err != nil {
				p.Log.Error("outbox tick", zap.Error(err))
			}
		}
	}
}

func (p *Publisher) tick(ctx context.Context) error {
	// 1) 量 lag (老 pending 多旧) — 报指标
	if err := p.measureLag(ctx); err != nil {
		p.Log.Warn("outbox lag measure", zap.Error(err))
	}
	// 2) fetch + publish
	rows, err := p.DB.QueryContext(ctx, `
		SELECT event_id, aggregate, aggregate_id, event_type, payload, topic, headers_json, retry_count, created_at
		  FROM tx_outbox
		 WHERE status = 'pending'
		 ORDER BY created_at
		 LIMIT ?`, p.BatchSize)
	if err != nil {
		return err
	}
	defer rows.Close()

	events := make([]Event, 0, p.BatchSize)
	for rows.Next() {
		var e Event
		var headersJSON []byte
		if err := rows.Scan(&e.EventID, &e.Aggregate, &e.AggregateID, &e.EventType,
			&e.Payload, &e.Topic, &headersJSON, &e.RetryCount, &e.CreatedAt); err != nil {
			return err
		}
		if len(headersJSON) > 0 {
			_ = json.Unmarshal(headersJSON, &e.Headers)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range events {
		p.publishOne(ctx, e)
	}
	return nil
}

func (p *Publisher) publishOne(ctx context.Context, e Event) {
	err := p.Publish(ctx, e)
	if err == nil {
		_, _ = p.DB.ExecContext(ctx, `
			UPDATE tx_outbox SET status='published', published_at=NOW(3), last_error=NULL
			 WHERE event_id=?`, e.EventID)
		if p.mPublished != nil {
			p.mPublished.Inc()
		}
		return
	}
	// 失败 — 更新 retry_count, 决定继续还是 DLQ
	newRetry := e.RetryCount + 1
	last := truncate(err.Error(), 1000)
	if p.FailedDLQ && newRetry >= p.MaxRetries {
		_, _ = p.DB.ExecContext(ctx, `
			UPDATE tx_outbox SET status='failed', retry_count=?, last_error=?
			 WHERE event_id=?`, newRetry, last, e.EventID)
		if p.mFailed != nil {
			p.mFailed.Inc()
		}
		p.Log.Error("outbox event moved to DLQ",
			zap.String("event_id", e.EventID), zap.Int("retries", newRetry), zap.Error(err))
		return
	}
	_, _ = p.DB.ExecContext(ctx, `
		UPDATE tx_outbox SET retry_count=?, last_error=?
		 WHERE event_id=?`, newRetry, last, e.EventID)
	p.Log.Warn("outbox publish failed (will retry)",
		zap.String("event_id", e.EventID), zap.Int("retries", newRetry), zap.Error(err))
}

func (p *Publisher) measureLag(ctx context.Context) error {
	if p.mLag == nil {
		return nil
	}
	var oldest sql.NullTime
	err := p.DB.QueryRowContext(ctx, `
		SELECT MIN(created_at) FROM tx_outbox WHERE status='pending'`).Scan(&oldest)
	if err != nil {
		return err
	}
	if oldest.Valid {
		p.mLag.Set(time.Since(oldest.Time).Seconds())
	} else {
		p.mLag.Set(0)
	}
	return nil
}

// ReplayFailedDLQ 把 failed 状态的事件重置为 pending — admin 触发.
// 返回重置笔数.
func ReplayFailedDLQ(ctx context.Context, db *sql.DB, eventIDs []string) (int, error) {
	if len(eventIDs) == 0 {
		return 0, nil
	}
	placeholders := ""
	args := make([]any, 0, len(eventIDs))
	for i, id := range eventIDs {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	res, err := db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE tx_outbox SET status='pending', retry_count=0, last_error=NULL
		 WHERE event_id IN (%s) AND status='failed'`, placeholders), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CleanupPublished 7d 之前的 published 行清掉, 控表大小.
func CleanupPublished(ctx context.Context, db *sql.DB, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.ExecContext(ctx, `
		DELETE FROM tx_outbox WHERE status='published' AND published_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

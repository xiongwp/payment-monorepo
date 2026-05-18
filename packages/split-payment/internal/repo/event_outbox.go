// event_outbox.go — SP-AC-7 L5: 事件发布的 transactional outbox.
//
// 之前 engine.publishEvent 是 fire-and-forget — Kafka producer 出错只 log warn,
// 下游订阅者可能漏接 (transfer.posted / payout.paid / reversal.succeeded 之类).
//
// 改造:
//   - 业务 tx 内 → INSERT event_outbox 行 (跟业务数据原子)
//   - 后台 worker 扫 outbox → Produce 到 Kafka → ACK 后 mark sent
//   - 失败保留 pending, 下次重试; 重试达上限 → dead_letter
//
// 这是经典 "Outbox Pattern", 保证 "DB commit 且 Kafka 发送" 等价于 at-least-once 语义.
//
// 与 R5 ReversalOutbox 设计高度相似, 但作用面更广 (任意 event), schema 更通用.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EnsureEventOutboxSchema 启动期建表.
func EnsureEventOutboxSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS event_outbox (
			id            BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			event_type    VARCHAR(64) NOT NULL,
			payload_json  JSON NOT NULL,
			status        VARCHAR(32) NOT NULL DEFAULT 'pending', -- pending/sent/dead_letter
			retry_count   INT NOT NULL DEFAULT 0,
			max_retry     INT NOT NULL DEFAULT 10,
			last_error    TEXT,
			next_retry_at DATETIME NOT NULL,
			created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			KEY idx_status_next (status, next_retry_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	return err
}

// EventOutboxRow.
type EventOutboxRow struct {
	ID          int64
	EventType   string
	PayloadJSON []byte
	RetryCount  int
	MaxRetry    int
}

// EventOutbox 操作封装.
type EventOutbox struct {
	DB *sql.DB
}

// Enqueue 把事件写进 outbox. 业务侧应该在跟主数据一个 tx 里调; 这里不强制 tx,
// 让调用方决定 atomicity (跟 ApplyReversalAtomic 一样: 调用方拿 tx 包).
//
// 当前实现走独立 DB 调用, 后续优化为 tx-aware.
func (o *EventOutbox) Enqueue(ctx context.Context, eventType string, payloadJSON []byte) error {
	if o.DB == nil {
		return errors.New("nil db")
	}
	_, err := o.DB.ExecContext(ctx, `
		INSERT INTO event_outbox (event_type, payload_json, next_retry_at)
		VALUES (?, ?, NOW())`, eventType, payloadJSON)
	return err
}

// Claim 从 pending 行里捞一批, CAS 推后 next_retry_at 防并发 worker 重复处理.
func (o *EventOutbox) Claim(ctx context.Context, limit int) ([]EventOutboxRow, error) {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	rows, err := tx.QueryContext(ctx, `
		SELECT id, event_type, payload_json, retry_count, max_retry
		  FROM event_outbox
		 WHERE status='pending' AND next_retry_at < NOW()
		 ORDER BY next_retry_at ASC
		 LIMIT ?
		 FOR UPDATE`, limit)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("claim select: %w", err)
	}
	var out []EventOutboxRow
	for rows.Next() {
		var r EventOutboxRow
		if err := rows.Scan(&r.ID, &r.EventType, &r.PayloadJSON, &r.RetryCount, &r.MaxRetry); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	for _, r := range out {
		backoff := time.Duration(10*(1<<uint(r.RetryCount))) * time.Second
		if backoff > 10*time.Minute {
			backoff = 10 * time.Minute
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE event_outbox SET next_retry_at=?, retry_count=retry_count+1 WHERE id=?`,
			time.Now().Add(backoff), r.ID); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return out, nil
}

// MarkSent 成功发送后.
func (o *EventOutbox) MarkSent(ctx context.Context, id int64) error {
	_, err := o.DB.ExecContext(ctx, `UPDATE event_outbox SET status='sent' WHERE id=?`, id)
	return err
}

// MarkDeadLetter 重试达上限.
func (o *EventOutbox) MarkDeadLetter(ctx context.Context, id int64, lastErr string) error {
	_, err := o.DB.ExecContext(ctx,
		`UPDATE event_outbox SET status='dead_letter', last_error=? WHERE id=?`, lastErr, id)
	return err
}

// UpdateError 记录这次错误但继续 pending.
func (o *EventOutbox) UpdateError(ctx context.Context, id int64, lastErr string) error {
	_, err := o.DB.ExecContext(ctx,
		`UPDATE event_outbox SET last_error=? WHERE id=?`, lastErr, id)
	return err
}

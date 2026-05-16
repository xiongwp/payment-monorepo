// Package routing — db_retry_queue.go：基于 MySQL 的 RetryQueue 真实实现。
//
// 替代 retry_queue.go 中的 DBRetryQueue 占位符。
//
// 表结构见 schema_retry_queue.sql。设计要点：
//   - 单条 UPDATE ... WHERE state='pending' AND next_retry_at <= NOW() ... LIMIT N 拿走任务
//     (用 row-level lock + state 转 'leased' 防并发 worker 抢)
//   - lease_owner / lease_expires_at 兜底进程崩溃 → 过期 lease 回 pending
//   - 终态 MarkSuccess → state='done',保留 7 天后 cron 物理删除 (审计)
//   - MarkRetry 写 next_retry_at + last_error,attempt++,state 回 pending
//   - 入队 UNIQUE(payment_intent_id, idempotency_key) → 重复入队幂等
package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
)

// LeaseDuration: leased 状态下的 worker 占用时长,超过则被其他 worker 抢
const LeaseDuration = 5 * time.Minute

// DBRetryQueueImpl 真实数据库实现 (取代 retry_queue.go 中的占位符)
type DBRetryQueueImpl struct {
	db     *sql.DB
	logger *zap.Logger
	owner  string // 本实例 ID (hostname + pid),用于 lease 排他
}

// NewDBRetryQueue 创建 DB-backed retry queue.
//
// 调用方负责传入已配置的 *sql.DB (DSN / 连接池等)。
func NewDBRetryQueue(db *sql.DB, logger *zap.Logger) *DBRetryQueueImpl {
	host, _ := os.Hostname()
	return &DBRetryQueueImpl{
		db:     db,
		logger: logger,
		owner:  fmt.Sprintf("%s-%d", host, os.Getpid()),
	}
}

// Enqueue 入队 (幂等)。UNIQUE(payment_intent_id, idempotency_key) → 重复入忽略。
func (q *DBRetryQueueImpl) Enqueue(ctx context.Context, task *RetryTask) error {
	if task == nil || task.ID == "" || task.IdempotencyKey == "" {
		return errors.New("invalid task: id and idempotency_key required")
	}
	if task.NextRetryAt.IsZero() {
		task.NextRetryAt = time.Now().Add(30 * time.Second)
	}

	metaJSON := []byte("{}")
	if len(task.Metadata) > 0 {
		var err error
		metaJSON, err = json.Marshal(task.Metadata)
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
	}

	const q1 = `
INSERT INTO payment_retry_queue
    (id, payment_intent_id, idempotency_key, amount, currency, payment_method,
     country, bin, failed_adapter, reason, attempt, next_retry_at,
     last_error_msg, metadata, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', NOW(), NOW())
ON DUPLICATE KEY UPDATE
    -- 已存在则不动 (幂等)。如果想刷 next_retry_at,可放开:
    -- next_retry_at = LEAST(next_retry_at, VALUES(next_retry_at)),
    updated_at = NOW()
`
	_, err := q.db.ExecContext(ctx, q1,
		task.ID, task.PaymentIntentID, task.IdempotencyKey,
		task.Amount, task.Currency, task.PaymentMethod,
		task.Country, task.BIN, task.FailedAdapter, task.Reason,
		task.Attempt, task.NextRetryAt, task.LastErrorMsg, metaJSON,
	)
	if err != nil {
		return fmt.Errorf("retry queue insert: %w", err)
	}
	return nil
}

// Dequeue 原子地把一批 pending 任务转为 leased 并返回 (并发 worker 安全)。
//
// 两步走:
//  1. UPDATE ... SET state='leased', lease_owner=?, lease_expires_at=NOW()+5m
//     WHERE state IN ('pending', 'leased_expired')
//     AND next_retry_at <= NOW()
//     ORDER BY next_retry_at LIMIT ?
//  2. SELECT * FROM payment_retry_queue WHERE lease_owner=? AND state='leased'
func (q *DBRetryQueueImpl) Dequeue(ctx context.Context, limit int) ([]*RetryTask, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	// 先把过期 lease 回收 (worker 死掉场景)
	if _, err := q.db.ExecContext(ctx, `
UPDATE payment_retry_queue
SET state='pending', lease_owner=NULL, lease_expires_at=NULL
WHERE state='leased' AND lease_expires_at < NOW()
`); err != nil && q.logger != nil {
		q.logger.Warn("reclaim expired lease failed", zap.Error(err))
	}

	tx, err := q.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 抢占租约
	res, err := tx.ExecContext(ctx, `
UPDATE payment_retry_queue
SET state='leased',
    lease_owner=?,
    lease_expires_at=DATE_ADD(NOW(), INTERVAL ? SECOND),
    updated_at=NOW()
WHERE state='pending'
  AND next_retry_at <= NOW()
ORDER BY next_retry_at ASC
LIMIT ?
`, q.owner, int(LeaseDuration.Seconds()), limit)
	if err != nil {
		return nil, fmt.Errorf("lease tasks: %w", err)
	}
	leased, _ := res.RowsAffected()
	if leased == 0 {
		_ = tx.Commit()
		return nil, nil
	}

	rows, err := tx.QueryContext(ctx, `
SELECT id, payment_intent_id, idempotency_key, amount, currency, payment_method,
       country, bin, failed_adapter, reason, attempt, next_retry_at,
       last_error_msg, metadata, created_at, updated_at
FROM payment_retry_queue
WHERE state='leased' AND lease_owner=?
ORDER BY next_retry_at ASC
LIMIT ?
`, q.owner, limit)
	if err != nil {
		return nil, fmt.Errorf("select leased: %w", err)
	}
	defer rows.Close()

	var tasks []*RetryTask
	for rows.Next() {
		t := &RetryTask{}
		var metaJSON sql.NullString
		if err := rows.Scan(
			&t.ID, &t.PaymentIntentID, &t.IdempotencyKey,
			&t.Amount, &t.Currency, &t.PaymentMethod,
			&t.Country, &t.BIN, &t.FailedAdapter, &t.Reason,
			&t.Attempt, &t.NextRetryAt, &t.LastErrorMsg, &metaJSON,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if metaJSON.Valid && metaJSON.String != "" && metaJSON.String != "{}" {
			t.Metadata = map[string]string{}
			_ = json.Unmarshal([]byte(metaJSON.String), &t.Metadata)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return tasks, nil
}

// MarkRetry 记一次失败重试 — attempt++ / 写 next_retry_at / 解 lease 回 pending
func (q *DBRetryQueueImpl) MarkRetry(
	ctx context.Context,
	taskID string,
	attempt int,
	nextRetryAt time.Time,
	errorMsg string,
) error {
	// 错误消息截断,防止超长 TEXT (生产 1KB 够用)
	if len(errorMsg) > 1024 {
		errorMsg = errorMsg[:1021] + "..."
	}
	res, err := q.db.ExecContext(ctx, `
UPDATE payment_retry_queue
SET state='pending',
    lease_owner=NULL,
    lease_expires_at=NULL,
    attempt=?,
    next_retry_at=?,
    last_error_msg=?,
    updated_at=NOW()
WHERE id=? AND state IN ('leased','pending')
`, attempt, nextRetryAt, errorMsg, taskID)
	if err != nil {
		return fmt.Errorf("mark retry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task not found or state mismatch: %s", taskID)
	}
	return nil
}

// MarkSuccess 标记终态。物理删除留给 cron (审计需要)。
func (q *DBRetryQueueImpl) MarkSuccess(ctx context.Context, taskID string) error {
	res, err := q.db.ExecContext(ctx, `
UPDATE payment_retry_queue
SET state='done',
    lease_owner=NULL,
    lease_expires_at=NULL,
    updated_at=NOW()
WHERE id=? AND state IN ('leased','pending')
`, taskID)
	if err != nil {
		return fmt.Errorf("mark success: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task not found or state mismatch: %s", taskID)
	}
	return nil
}

// Stats 队列健康度 (Prometheus 抓)
func (q *DBRetryQueueImpl) Stats(ctx context.Context) (map[string]int, error) {
	rows, err := q.db.QueryContext(ctx, `
SELECT state, COUNT(*) FROM payment_retry_queue
WHERE state IN ('pending','leased','done')
GROUP BY state
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{"pending": 0, "leased": 0, "done": 0, "overdue": 0}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	// 还要算 overdue (next_retry_at 已过但仍 pending)
	var overdue int
	if err := q.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM payment_retry_queue WHERE state='pending' AND next_retry_at <= NOW()
`).Scan(&overdue); err != nil {
		return nil, err
	}
	out["overdue"] = overdue
	return out, nil
}

// PurgeDoneOlderThan 物理删除已完成且超过保留期的记录 (cron 每日凌晨调用)
func (q *DBRetryQueueImpl) PurgeDoneOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	cutoff := time.Now().Add(-age)
	res, err := q.db.ExecContext(ctx, `
DELETE FROM payment_retry_queue WHERE state='done' AND updated_at < ?
`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if q.logger != nil {
		q.logger.Info("purged done retry tasks", zap.Int64("rows", n))
	}
	return n, nil
}

// helper: 构造 schema migration 用 DDL (用于测试)
//
//nolint:unused // 仅 test 使用
func ddlPaymentRetryQueue() string {
	// 实际 DDL 在 schema_retry_queue.sql,这里只是个保险,避免漏导致 import
	const ddl = `CREATE TABLE IF NOT EXISTS payment_retry_queue (...)`
	return strings.TrimSpace(ddl)
}

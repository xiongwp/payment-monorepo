// reversal_outbox.go — SP-AC-7 R5: Reversal 失败重试 outbox.
//
// 之前 refund.go 里 ApplyReversalAtomic 失败仅 mark `Status=failed` + log error, 没重试.
// 这意味着如果失败原因是瞬态 (lock wait / DB connection drop), 数据就此停留 failed 状态
// 直到运营介入. 资金安全角度: reversed_amount 还没加回 transfer, 等于"客户退了但 transfer
// 状态没更新", 后续退款累计计算会出错.
//
// 设计:
//   - 失败的 ApplyReversal 调用 → INSERT reversal_retry_outbox 行 (reversal_id 唯一)
//   - 独立 worker 周期扫 outbox 行重试 (退避 backoff)
//   - 重试成功 → 标 outbox row done; 重试达到上限 → 标 dead-letter 让运营介入
//
// 跟 accounting 端 outbox 相似但是 split-payment 自治, 不依赖外部 broker.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// EnsureReversalOutboxSchema 启动期建表 (幂等).
func EnsureReversalOutboxSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS reversal_retry_outbox (
			id              BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			reversal_id     VARCHAR(64) NOT NULL,
			transfer_id     VARCHAR(64) NOT NULL,
			delta_minor     BIGINT NOT NULL,
			status          VARCHAR(32) NOT NULL DEFAULT 'pending',  -- pending/done/dead_letter
			retry_count     INT NOT NULL DEFAULT 0,
			max_retry       INT NOT NULL DEFAULT 5,
			last_error      TEXT,
			next_retry_at   DATETIME NOT NULL,
			created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			UNIQUE KEY uk_reversal_id (reversal_id),
			KEY idx_status_next (status, next_retry_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`)
	return err
}

// ReversalOutboxRow 是 outbox 表的一行.
type ReversalOutboxRow struct {
	ID          int64
	ReversalID  string
	TransferID  string
	DeltaMinor  int64
	Status      string
	RetryCount  int
	MaxRetry    int
	LastError   string
	NextRetryAt time.Time
}

// ReversalOutbox 操作 outbox 表的封装.
type ReversalOutbox struct {
	DB  *sql.DB
	Log *zap.Logger
}

// Enqueue — refund.go ApplyReversalAtomic 失败时调.
// 幂等 by reversal_id; 重复 enqueue 同一 reversal_id 静默更新 last_error.
func (o *ReversalOutbox) Enqueue(ctx context.Context, reversalID, transferID string, deltaMinor int64, lastErr error) error {
	if o.DB == nil {
		return errors.New("nil db")
	}
	errMsg := ""
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	_, err := o.DB.ExecContext(ctx, `
		INSERT INTO reversal_retry_outbox
		(reversal_id, transfer_id, delta_minor, status, retry_count, last_error, next_retry_at)
		VALUES (?, ?, ?, 'pending', 0, ?, ?)
		ON DUPLICATE KEY UPDATE
			last_error = VALUES(last_error),
			updated_at = CURRENT_TIMESTAMP`,
		reversalID, transferID, deltaMinor, errMsg, time.Now().Add(15*time.Second))
	return err
}

// Claim 拿出一批 pending 且 next_retry_at < now 的 row, CAS 锁住后返回.
//
// 实现: 用单条 UPDATE 把 pending 行的 retry_count++ + next_retry_at 推后, 再 SELECT;
// 这样并发 worker 不会重复处理同一行.
//
// 简化版用 SELECT FOR UPDATE + UPDATE 两步, 同事务. 真生产可换 leasing.
func (o *ReversalOutbox) Claim(ctx context.Context, limit int) ([]ReversalOutboxRow, error) {
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
		SELECT id, reversal_id, transfer_id, delta_minor, retry_count, max_retry, COALESCE(last_error,'')
		  FROM reversal_retry_outbox
		 WHERE status='pending' AND next_retry_at < NOW()
		 ORDER BY next_retry_at ASC
		 LIMIT ?
		 FOR UPDATE`, limit)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("select claim: %w", err)
	}
	var out []ReversalOutboxRow
	for rows.Next() {
		var r ReversalOutboxRow
		if err := rows.Scan(&r.ID, &r.ReversalID, &r.TransferID, &r.DeltaMinor,
			&r.RetryCount, &r.MaxRetry, &r.LastError); err != nil {
			rows.Close()
			_ = tx.Rollback()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	// 推迟下一次 retry (即使本次失败也避免 hot loop). 退避: 30s * 2^retry_count, max 30min.
	for _, r := range out {
		backoff := time.Duration(30*(1<<uint(r.RetryCount))) * time.Second
		if backoff > 30*time.Minute {
			backoff = 30 * time.Minute
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE reversal_retry_outbox SET next_retry_at=?, retry_count=retry_count+1 WHERE id=?`,
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

// MarkDone 重试成功后标记完成.
func (o *ReversalOutbox) MarkDone(ctx context.Context, id int64) error {
	_, err := o.DB.ExecContext(ctx,
		`UPDATE reversal_retry_outbox SET status='done' WHERE id=?`, id)
	return err
}

// MarkDeadLetter 重试达上限, 标 dead_letter 让运营介入.
func (o *ReversalOutbox) MarkDeadLetter(ctx context.Context, id int64, lastErr string) error {
	_, err := o.DB.ExecContext(ctx,
		`UPDATE reversal_retry_outbox SET status='dead_letter', last_error=? WHERE id=?`, lastErr, id)
	return err
}

// UpdateError 记录本次重试的错误 (但不标 dead_letter, 还有 retry 余额).
func (o *ReversalOutbox) UpdateError(ctx context.Context, id int64, lastErr string) error {
	_, err := o.DB.ExecContext(ctx,
		`UPDATE reversal_retry_outbox SET last_error=? WHERE id=?`, lastErr, id)
	return err
}

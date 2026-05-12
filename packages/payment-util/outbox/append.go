// append.go — 跟业务行同事务写 outbox.
//
// 关键: 调用方必须传入 *sql.Tx (不是 *sql.DB), 否则 dual-write 风险回来.
// 如果 tx 没 Commit, INSERT 会跟业务行一起回滚 — 这就是 outbox 模式的全部精髓.

package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Append 在传入 tx 里 INSERT 一条 pending event.
//
// tx.Commit 之后再调用 publisher 才能感知 (publisher 用单独 connection 扫表).
//
// 表名: tx_outbox (跟 schema.sql 一致); 可通过 EnvVar OUTBOX_TABLE 覆盖.
func Append(ctx context.Context, tx *sql.Tx, evt Event) error {
	if err := evt.Validate(); err != nil {
		return err
	}
	var headersJSON []byte
	if len(evt.Headers) > 0 {
		var err error
		headersJSON, err = json.Marshal(evt.Headers)
		if err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO tx_outbox
		  (event_id, aggregate, aggregate_id, event_type, payload, topic, headers_json, created_at, status, retry_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0)`,
		evt.EventID,
		evt.Aggregate,
		evt.AggregateID,
		evt.EventType,
		evt.Payload,
		evt.Topic,
		headersJSON,
		evt.CreatedAt,
	)
	return err
}

// AppendDB 不在 tx 里 append — only for non-critical events (audit/observability).
// 一般禁用; 调用前请确认能接受 dual-write 风险.
func AppendDB(ctx context.Context, db *sql.DB, evt Event) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := Append(ctx, tx, evt); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

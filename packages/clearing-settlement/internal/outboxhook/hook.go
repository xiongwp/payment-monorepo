// Package outboxhook — clearing-settlement 接 payment-util/outbox 的桥.
//
// 接入点 (业务侧 service/payout.go):
//
//   tx, _ := db.BeginTx(ctx, nil)
//   defer tx.Rollback()
//
//   // 业务写
//   if _, err := tx.ExecContext(ctx, "INSERT INTO payouts ...", ...); err != nil {
//       return err
//   }
//
//   // 同事务写 outbox
//   if err := outboxhook.AppendPayoutEvent(ctx, tx, "PayoutSettled", payout); err != nil {
//       return err
//   }
//   return tx.Commit()
//
// Publisher 单独 goroutine 扫表; 配合 cmd/server/main.go 启动 (见 PublishLoop).
//
// 这个 package 是 clearing-settlement 内部胶水, 隔离 outbox 库依赖, 让业务代码干净.

package outboxhook

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Event 跟 payment-util/outbox.Event 字段对齐, 但本地副本避免跨包依赖.
type Event struct {
	EventID     string
	Aggregate   string  // "payout"
	AggregateID string  // payout_id
	EventType   string  // PayoutCreated / PayoutSettled / PayoutFailed
	Payload     []byte
	Topic       string
	Headers     map[string]string
}

// AppendPayoutEvent 跟业务 tx 同事务写一条 outbox 行.
//
// eventType 列表:
//   "PayoutCreated"     -- 业务侧 INSERT 后立即触发, 下游 tax-reporting / accounting / merchant-webhook 都订阅
//   "PayoutApproved"    -- ops 通过 approval-service 后
//   "PayoutSent"        -- 真发给 bank rail 之后 (rails/ payload 已生成)
//   "PayoutSettled"     -- 收 bank 回执 (reconplatform 触发)
//   "PayoutFailed"      -- 各种失败
//   "PayoutReversed"    -- ACH return / SEPA R-message
func AppendPayoutEvent(ctx context.Context, tx *sql.Tx, eventType string, payout interface{}) error {
	payload, err := json.Marshal(payout)
	if err != nil {
		return fmt.Errorf("outboxhook marshal: %w", err)
	}
	aggregateID := ""
	// 反射拿 ID 字段; 简化版用 map 兜底
	if m, ok := payout.(map[string]interface{}); ok {
		if id, ok := m["id"].(string); ok {
			aggregateID = id
		} else if id, ok := m["payout_id"].(string); ok {
			aggregateID = id
		}
	}
	if aggregateID == "" {
		aggregateID = "unknown"
	}

	eventID := "evt_" + randHex(16)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO tx_outbox
		  (event_id, aggregate, aggregate_id, event_type, payload, topic, headers_json, created_at, status, retry_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 0)`,
		eventID,
		"payout",
		aggregateID,
		eventType,
		payload,
		"payments.payouts",
		nil,
		time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("outboxhook insert: %w", err)
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

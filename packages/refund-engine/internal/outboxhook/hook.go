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

// AppendRefundEvent 同 clearing-settlement.outboxhook.AppendPayoutEvent.
func AppendRefundEvent(ctx context.Context, tx *sql.Tx, eventType string, refund interface{}) error {
	payload, err := json.Marshal(refund)
	if err != nil {
		return fmt.Errorf("outboxhook marshal: %w", err)
	}
	aggregateID := ""
	if m, ok := refund.(map[string]interface{}); ok {
		if id, ok := m["id"].(string); ok {
			aggregateID = id
		} else if id, ok := m["refund_id"].(string); ok {
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
		eventID, "refund", aggregateID, eventType, payload, "payments.refunds", nil, time.Now().UTC(),
	)
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

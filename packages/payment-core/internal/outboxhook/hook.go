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

// AppendChargeEvent 同 refund-engine / clearing-settlement 模式.
func AppendChargeEvent(ctx context.Context, tx *sql.Tx, eventType string, charge interface{}) error {
	payload, err := json.Marshal(charge)
	if err != nil {
		return fmt.Errorf("outboxhook marshal: %w", err)
	}
	aggregateID := ""
	if m, ok := charge.(map[string]interface{}); ok {
		if id, ok := m["id"].(string); ok {
			aggregateID = id
		} else if id, ok := m["charge_id"].(string); ok {
			aggregateID = id
		} else if id, ok := m["payment_intent_id"].(string); ok {
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
		eventID, "charge", aggregateID, eventType, payload, "payments.charges", nil, time.Now().UTC(),
	)
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

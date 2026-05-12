// Package audit — vault 操作审计. PAN 永不写 audit; 写 internal_token + last4.

package audit

import (
	"context"
	"time"

	"go.uber.org/zap"
)

type Event struct {
	OccurredAt time.Time              `json:"occurred_at"`
	Actor      string                 `json:"actor"`
	Action     string                 `json:"action"` // exchange / provision / charge_intent / suspend / delete
	Token      string                 `json:"token"`  // internal_token
	Details    map[string]interface{} `json:"details"`
	SourceIP   string                 `json:"source_ip"`
}

type Sink interface {
	Emit(ctx context.Context, e Event) error
}

type LogSink struct{ L *zap.Logger }

func (l LogSink) Emit(_ context.Context, e Event) error {
	l.L.Info("vault_audit",
		zap.String("actor", e.Actor),
		zap.String("action", e.Action),
		zap.String("token", e.Token),
		zap.Any("details", e.Details),
		zap.String("source_ip", e.SourceIP),
		zap.Time("occurred_at", e.OccurredAt),
	)
	return nil
}

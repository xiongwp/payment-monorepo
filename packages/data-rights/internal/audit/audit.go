// Package audit — data-rights 操作审计.
//
// 高敏感操作:
//   - submit (用户提交)
//   - verify (身份验证通过)
//   - approve (ops 批准)
//   - reject (ops 拒绝, 必须有 reason)
//   - fulfilled (执行完成 — export 发送 / erasure 完成)
//
// 所有这些必须留 hash 链 audit (反内部作恶) → 接 audit-log 服务.

package audit

import (
	"context"
	"time"

	"go.uber.org/zap"
)

type Event struct {
	OccurredAt time.Time              `json:"occurred_at"`
	Actor      string                 `json:"actor"`
	Action     string                 `json:"action"`
	RequestID  string                 `json:"request_id"`
	Details    map[string]interface{} `json:"details"`
	SourceIP   string                 `json:"source_ip"`
}

type Sink interface {
	Emit(ctx context.Context, e Event) error
}

type LogSink struct{ L *zap.Logger }

func (l LogSink) Emit(_ context.Context, e Event) error {
	l.L.Info("data_rights_audit",
		zap.String("actor", e.Actor),
		zap.String("action", e.Action),
		zap.String("request_id", e.RequestID),
		zap.Any("details", e.Details),
		zap.String("source_ip", e.SourceIP),
		zap.Time("occurred_at", e.OccurredAt),
	)
	return nil
}

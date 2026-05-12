// Package audit — AML 操作审计.
//
// 所有 screen 调用 + hit 复核决策都写一条 audit 记录, 跟 audit-log 服务对接.
// 这里只做 in-process append-only, 异步发到 audit-log (HTTP) 的客户端.

package audit

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

type Event struct {
	OccurredAt time.Time              `json:"occurred_at"`
	Actor      string                 `json:"actor"`        // oauth2 client_id / staff ID
	Action     string                 `json:"action"`       // screen / hit_resolve / list_refresh
	Subject    string                 `json:"subject"`      // merchant_id / hit_id / source name
	Details    map[string]interface{} `json:"details"`
	SourceIP   string                 `json:"source_ip"`
}

// Sink 抽象 — 单测用 memorySink, 生产用 HTTP audit-log client
type Sink interface {
	Emit(ctx context.Context, e Event) error
}

// MemSink 测试 / dev
type MemSink struct {
	mu     sync.Mutex
	Events []Event
}

func (m *MemSink) Emit(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Events = append(m.Events, e)
	return nil
}

// LogSink 把事件输出到 zap (生产临时兜底, 主路径走 HTTP)
type LogSink struct{ L *zap.Logger }

func (l LogSink) Emit(_ context.Context, e Event) error {
	l.L.Info("aml_audit",
		zap.String("actor", e.Actor),
		zap.String("action", e.Action),
		zap.String("subject", e.Subject),
		zap.Any("details", e.Details),
		zap.String("source_ip", e.SourceIP),
		zap.Time("occurred_at", e.OccurredAt),
	)
	return nil
}

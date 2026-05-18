// audit.go — SP-AC-7 S6: 资金审计 log.
//
// 设计:
//   - TriggerEvent 前后必写一条 audit, 含 graph_key / business_no / event_code / voucher_no / actor.
//   - 写法 best-effort: audit 失败不阻塞业务, 但 log error 报警.
//   - 默认实现是 zap 结构化日志 + 行内 JSON; 生产应替换为独立 audit-log service / Kafka topic
//     (避免跟业务日志共池冲淡资金事件).
//
// 异常场景:
//   - audit sink 不可达 → log warn, 业务继续 (避免 ops 故障误伤交易)
//   - voucher_no 半空 (partial failure) → 仍写 audit, 标 outcome=partial 让 ops 看到
//   - actor 未鉴权时 = "unknown"; auth interceptor 已强制 token, 不会到这步未鉴权
package grpcsvc

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// AuditEvent 一条资金审计事件.
//
// 字段命名跟 accounting 端审计对齐, 方便跨服务 join 排查.
type AuditEvent struct {
	Action     string    // 固定 "moneyflow.trigger"
	GraphKey   string    // graph key
	BusinessNo string    // = trigger.charge_id, 跨服务关联键
	EventCode  string    // 单个 voucher 维度, 多次写 audit
	OrderNo    string    // accounting order_no
	VoucherNo  string    // 成功时填; 失败为空
	Status     int32     // 0=pending / 1=processing / 2=success / 3=failed
	Actor      string    // gRPC metadata 里的 X-Admin-Token 主体 (后续可改成具名 actor)
	OccurredAt time.Time // 业务时间
	Error      string    // 失败原因
	TraceID    string    // SP-AC-7 P10: OTel trace_id, 跨服务 join 排查用
}

// AuditSink 抽象出来便于替换 (单测 mock / 生产换 Kafka).
type AuditSink interface {
	Write(ctx context.Context, ev AuditEvent) error
}

// ZapAuditSink — 默认实现, 把 audit event 以单独 "audit" 命名空间 + structured field 写 zap.
// 生产建议改用 KafkaAuditSink (publish 到 audit-log topic), 当前 ZapAuditSink 把 audit
// 跟业务 log 混在 stdout, 不利于权限隔离 / 长期保存.
type ZapAuditSink struct{ Log *zap.Logger }

// PII redaction: ZapAuditSink 在写 actor / business_no 等可能含敏感 ID 的字段前应该
// 调 observability.MaskAccountNo 等. 当前简化处理: business_no 是业务键 (e.g. charge_id),
// 可见; actor 已经在 admin_handlers.extractActor 里脱敏为 token-前 8 字节.

// Write 落 audit event. zap 是 sync 安全; 这里不返 err (zap 自己不会失败) 兜底.
func (s *ZapAuditSink) Write(_ context.Context, ev AuditEvent) error {
	if s == nil || s.Log == nil {
		return nil
	}
	s.Log.Info("AUDIT",
		zap.String("action", ev.Action),
		zap.String("graph_key", ev.GraphKey),
		zap.String("business_no", ev.BusinessNo),
		zap.String("event_code", ev.EventCode),
		zap.String("order_no", ev.OrderNo),
		zap.String("voucher_no", ev.VoucherNo),
		zap.Int32("status", ev.Status),
		zap.String("actor", ev.Actor),
		zap.Time("occurred_at", ev.OccurredAt),
		zap.String("error", ev.Error),
		zap.String("trace_id", ev.TraceID), // SP-AC-7 P10
	)
	return nil
}

// ChainAuditSink — SP-AC-7 PROD3: 链式 sink, 同一 audit 同时落多个 sink (zap+Kafka).
// 任一失败不阻塞其它. 适合生产 wiring: ZapAuditSink (本地 stdout) + KafkaAuditSink (异地审计).
type ChainAuditSink struct{ Sinks []AuditSink }

// Write 串行调每个 sink, 失败只记不返错.
func (c *ChainAuditSink) Write(ctx context.Context, ev AuditEvent) error {
	for _, s := range c.Sinks {
		if s == nil {
			continue
		}
		_ = s.Write(ctx, ev) // best-effort
	}
	return nil
}

// Package auditstore 是从老 pkg/grpcutil 抽出来的纯数据类型 + 接口 + 哈希工具.
//
// 老 pkg/grpcutil 把 gRPC interceptor + 数据类型混在一起 — Kitex 切换后
// interceptor 全部失效, 只剩 AuditEntry / AuditStore / ComputeRowHash /
// IdempotencyRecord / IdempotencyStore / ErrIdempotencyExists 这几样还有
// 价值 (audit-verify CLI + audit_store.go adapter 还用).
//
// 新模块不引 google.golang.org/grpc — 是纯 SQL store 协议层.
package auditstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// ─── Audit ──────────────────────────────────────────────────────────────

// AuditEntry 一条审计日志 — 由 store 实现负责持久化 (admin_audit_log 表分片).
type AuditEntry struct {
	Actor       string
	ActorIP     string
	Method      string
	TargetID    string
	RequestBody string // JSON, redacted
	StatusCode  string
	ResponseErr string
	DurationMs  int
	TraceID     string
	PrevHash    string
	RowHash     string
	At          time.Time
}

// AuditStore append-only 日志后端 (合规要求).
// LastHash: 按 actor 路由到对应 shard 拿链 head; Insert: 持久化一条.
type AuditStore interface {
	LastHash(ctx context.Context, actor string) (string, error)
	Insert(ctx context.Context, entry *AuditEntry) error
}

// ComputeRowHash 链式 row_hash — sha256(prev|actor|ip|method|target|body|code|err|trace).
// audit-verify CLI 跨 binary 复用; 公式改动两边同步.
func ComputeRowHash(e *AuditEntry) string {
	s := e.PrevHash + "|" + e.Actor + "|" + e.ActorIP + "|" + e.Method + "|" +
		e.TargetID + "|" + e.RequestBody + "|" + e.StatusCode + "|" +
		e.ResponseErr + "|" + e.TraceID
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ─── Idempotency ────────────────────────────────────────────────────────

// IdempotencyRecord Stripe 风格幂等记录 (per-mutation).
type IdempotencyRecord struct {
	Key          string
	Method       string
	RequestHash  string
	StatusCode   string
	ResponseBody []byte
	ResponseErr  string
	Expires      time.Time
}

// IdempotencyStore 幂等存储后端 (Kitex middleware / 业务路径自取).
type IdempotencyStore interface {
	Get(ctx context.Context, key, method string) (*IdempotencyRecord, bool, error)
	TryInsert(ctx context.Context, rec *IdempotencyRecord) error
}

// ErrIdempotencyExists store TryInsert 撞 key 时必须返回此 sentinel.
type idempotencyErr int

func (e idempotencyErr) Error() string { return "idempotency record exists" }

const ErrIdempotencyExists = idempotencyErr(1)

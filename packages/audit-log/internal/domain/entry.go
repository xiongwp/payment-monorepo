// Package domain — 全局 audit_log 实体。
//
// 所有 admin web 操作进口（reconplatform / billing / clearing / dispute /
// kyc / accounting-admin 等）写一条 audit log 到本服务，hash chain 防篡改。
//
// 写入路径:
//   admin svc → POST /api/v1/audit/log
//                  body = AuditEntry
//                  → 服务端补 chain_hash + sequence_id 落库
//
// 防篡改:
//   chain_hash[N] = sha256(chain_hash[N-1] || canonical_json(entry[N]))
//   periodic 全量 VerifyChain 检测篡改
//   compliance 月底导 CSV → WORM 存储 (S3 Object Lock / Glacier)

package domain

import "time"

// AuditEntry 一条审计日志。
type AuditEntry struct {
	ID         int64     `db:"id" json:"id"`
	SequenceID int64     `db:"sequence_id" json:"sequence_id"`   // 全局自增 (chain 顺序)
	Service    string    `db:"service" json:"service"`           // 'reconplatform'/'billing-system'/'kyc-service' 等
	ActorEmail string    `db:"actor_email" json:"actor_email"`   // ops 真实邮箱
	ActorIP    string    `db:"actor_ip" json:"actor_ip,omitempty"`
	Action     string    `db:"action" json:"action"`             // 'diff.transition' / 'refund.approve' / 'fee_rule.create' ...
	ResourceType string  `db:"resource_type" json:"resource_type"` // 'diff' / 'refund' / 'fee_rule' / 'kyc_case' ...
	ResourceID string    `db:"resource_id" json:"resource_id"`
	Before     string    `db:"before" json:"before,omitempty"`   // JSON snapshot
	After      string    `db:"after" json:"after,omitempty"`     // JSON snapshot
	Note       string    `db:"note" json:"note,omitempty"`       // 操作人备注
	TraceID    string    `db:"trace_id" json:"trace_id,omitempty"`
	ChainHash  string    `db:"chain_hash" json:"chain_hash"`     // 服务端写入时填
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

// VerifyResult chain 校验结果。
type VerifyResult struct {
	OK         bool   `json:"ok"`
	TotalCount int64  `json:"total_count"`
	BadIndex   int64  `json:"bad_index,omitempty"`   // 从哪条开始链断（0 = 没断）
	Reason     string `json:"reason,omitempty"`
}

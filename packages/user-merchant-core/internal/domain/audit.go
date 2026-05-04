package domain

import "time"

// AdminAuditLog append-only 审计日志。row_hash 把前一行的 row_hash + 本行内容
// 摘在一起 sha256，形成链式签名；任何一行被后期篡改，它之后的 chain 全部对不上。
// 定时任务（或 CLI 工具）遍历验证。
type AdminAuditLog struct {
	ID          int64     `gorm:"column:id;primaryKey;autoIncrement"`
	Actor       string    `gorm:"column:actor;type:varchar(64)"`
	ActorIP     string    `gorm:"column:actor_ip;type:varchar(64)"`
	Method      string    `gorm:"column:method;type:varchar(128)"`
	TargetID    string    `gorm:"column:target_id;type:varchar(64)"`
	RequestBody string    `gorm:"column:request_body;type:mediumtext"`
	StatusCode  string    `gorm:"column:status_code;type:varchar(32)"`
	ResponseErr string    `gorm:"column:response_err;type:varchar(512)"`
	DurationMs  int       `gorm:"column:duration_ms"`
	TraceID     string    `gorm:"column:trace_id;type:varchar(64)"`
	PrevHash    string    `gorm:"column:prev_hash;type:char(64)"`
	RowHash     string    `gorm:"column:row_hash;type:char(64)"`
	Created     time.Time `gorm:"column:created;autoCreateTime"`
}

// TableName GORM
func (AdminAuditLog) TableName() string { return "admin_audit_log" }

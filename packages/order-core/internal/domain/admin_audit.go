package domain

import "time"

// AdminAuditLog append-only record of every admin mutation. Kept in the meta
// DB (non-sharded) so queries like "what did actor X do last week" are cheap
// and compliance tooling can dump the whole table.
//
// Intentionally no Update method on the repo — audit records are immutable.
type AdminAuditLog struct {
	ID           int64     `gorm:"column:id;primaryKey;autoIncrement"          json:"id"`
	Actor        string    `gorm:"column:actor;type:varchar(64)"               json:"actor"`
	ActorIP      string    `gorm:"column:actor_ip;type:varchar(64)"            json:"actor_ip,omitempty"`
	Action       string    `gorm:"column:action;type:varchar(64)"              json:"action"`
	TargetType   string    `gorm:"column:target_type;type:varchar(32)"         json:"target_type,omitempty"`
	TargetID     string    `gorm:"column:target_id;type:varchar(64)"           json:"target_id,omitempty"`
	HTTPMethod   string    `gorm:"column:http_method;type:varchar(8)"          json:"http_method,omitempty"`
	HTTPPath     string    `gorm:"column:http_path;type:varchar(256)"          json:"http_path,omitempty"`
	HTTPStatus   int       `gorm:"column:http_status"                          json:"http_status,omitempty"`
	RequestBody  string    `gorm:"column:request_body;type:mediumtext"         json:"request_body,omitempty"`
	ResponseCode string    `gorm:"column:response_code;type:varchar(32)"       json:"response_code,omitempty"`
	ResponseMsg  string    `gorm:"column:response_msg;type:varchar(512)"       json:"response_msg,omitempty"`
	DurationMs   int       `gorm:"column:duration_ms"                          json:"duration_ms,omitempty"`
	Created      time.Time `gorm:"column:created;autoCreateTime"               json:"created"`
}

// TableName GORM
func (AdminAuditLog) TableName() string { return "admin_audit_log" }

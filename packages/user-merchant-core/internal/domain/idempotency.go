package domain

import (
	"errors"
	"time"
)

// ErrIdempotencyMismatch 同 key 不同 body：可能是重放攻击或客户端 bug；
// 调用方应换一个 key 或修正 body。
var ErrIdempotencyMismatch = errors.New("idempotency key reused with different body")

// IdempotencyRecord (idempotency_key, method) → 响应缓存。同表跨方法不撞 key。
type IdempotencyRecord struct {
	Key          string    `gorm:"column:idempotency_key;primaryKey;type:varchar(128)"`
	Method       string    `gorm:"column:method;primaryKey;type:varchar(128)"`
	RequestHash  string    `gorm:"column:request_hash;type:char(64)"`
	StatusCode   string    `gorm:"column:status_code;type:varchar(32)"`
	ResponseBody []byte    `gorm:"column:response_body;type:mediumblob"`
	ResponseErr  string    `gorm:"column:response_err;type:varchar(512)"`
	Created      time.Time `gorm:"column:created;autoCreateTime"`
	Expires      time.Time `gorm:"column:expires"`
}

// TableName GORM
func (IdempotencyRecord) TableName() string { return "idempotency_key" }

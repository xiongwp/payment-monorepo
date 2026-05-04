package domain

import (
	"errors"
	"time"
)

// NotifyLogStatus 通知日志状态
type NotifyLogStatus string

const (
	NotifyLogStatusPending   NotifyLogStatus = "pending"   // 刚入队，还未发送
	NotifyLogStatusRetrying  NotifyLogStatus = "retrying"  // 已尝试但未成功，等下次 cron 扫描
	NotifyLogStatusSucceeded NotifyLogStatus = "succeeded" // 终态
	NotifyLogStatusFailed    NotifyLogStatus = "failed"    // 终态（达到 max_retries）
	NotifyLogStatusCanceled  NotifyLogStatus = "canceled"  // 运维主动取消
)

// ErrNotifyLogNotFound 通知日志不存在
var ErrNotifyLogNotFound = errors.New("notify_log not found")

// NotifyLog 对应 notify_log_XX 分片表（按 payment_intent_id 与 PI 同分片）。
//
// 每一次 "系统要告诉客户端一件事" 都落一条：PI 状态变化、Charge 结果、Refund 完成等。
// 同一 event 可能有多条（一个是商户 server HTTP 回调，一个是 iOS APNs 推送等）。
type NotifyLog struct {
	ID              string          `gorm:"column:id;primaryKey;type:varchar(64)"                  json:"id"`
	PaymentIntentID string          `gorm:"column:payment_intent_id;type:varchar(64);index:idx_pi" json:"payment_intent_id"`
	ChargeID        string          `gorm:"column:charge_id;type:varchar(64)"                      json:"charge_id,omitempty"`
	RefundID        string          `gorm:"column:refund_id;type:varchar(64)"                      json:"refund_id,omitempty"`
	EventID         string          `gorm:"column:event_id;type:varchar(64);index:idx_event"       json:"event_id"`
	EventType       string          `gorm:"column:event_type;type:varchar(64);index:idx_event_type" json:"event_type"`
	ClientType      string          `gorm:"column:client_type;type:varchar(16)"                    json:"client_type,omitempty"`
	NotifyChannel   string          `gorm:"column:notify_channel;type:varchar(16)"                 json:"notify_channel"`
	Target          string          `gorm:"column:target;type:varchar(512)"                        json:"target"`
	Payload         []byte          `gorm:"column:payload;type:blob"                               json:"-"`
	Status          NotifyLogStatus `gorm:"column:status;type:varchar(16);index:idx_status"        json:"status"`
	AttemptCount    int             `gorm:"column:attempt_count"                                   json:"attempt_count"`
	MaxRetries      int             `gorm:"column:max_retries"                                     json:"max_retries"`
	NextRetryAt     *time.Time      `gorm:"column:next_retry_at;index:idx_next_retry"              json:"next_retry_at,omitempty"`
	HTTPStatus      int             `gorm:"column:http_status"                                     json:"http_status,omitempty"`
	ErrorCode       string          `gorm:"column:error_code;type:varchar(64)"                     json:"error_code,omitempty"`
	ErrorMsg        string          `gorm:"column:error_msg;type:text"                             json:"error_msg,omitempty"`
	CompletedAt     *time.Time      `gorm:"column:completed_at"                                    json:"completed_at,omitempty"`
	Created         time.Time       `gorm:"column:created"                                         json:"created"`
	Updated         time.Time       `gorm:"column:updated"                                         json:"updated"`
}

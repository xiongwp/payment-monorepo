package domain

import (
	"errors"
	"time"
)

// ErrInboundWebhookDuplicate 入站 webhook 已经处理过（按 (channel_name, event_id) 唯一）
var ErrInboundWebhookDuplicate = errors.New("inbound webhook duplicate")

// InboundWebhookProcessStatus 处理状态
type InboundWebhookProcessStatus string

const (
	InboundWebhookPending    InboundWebhookProcessStatus = "pending"
	InboundWebhookSucceeded  InboundWebhookProcessStatus = "succeeded"
	InboundWebhookFailed     InboundWebhookProcessStatus = "failed"
	InboundWebhookDuplicate  InboundWebhookProcessStatus = "duplicate" // 同一 (channel, event_id) 第二次到达
)

// InboundWebhook 对应 inbound_webhook_XX 分片表（按 payment_intent_id 路由；
// 没有 PI 时按 event_id 哈希路由）。
//
// 用途：去重 channel webhook。同一事件 channel 可能重发（HTTP 重试、网络抖动、
// 或者 channel 自己的 at-least-once）。我们按 (channel_name, event_id) 唯一去重；
// 第二次插入会因唯一键冲突直接返回 ErrInboundWebhookDuplicate，调用方据此跳过
// 后续状态推进。
type InboundWebhook struct {
	ID              string                       `gorm:"column:id;primaryKey;type:varchar(64)"                            json:"id"`
	ChannelName     string                       `gorm:"column:channel_name;type:varchar(32);uniqueIndex:uk_channel_event" json:"channel_name"`
	EventID         string                       `gorm:"column:event_id;type:varchar(128);uniqueIndex:uk_channel_event"   json:"event_id"`
	EventType       string                       `gorm:"column:event_type;type:varchar(64)"                               json:"event_type"`
	PaymentIntentID string                       `gorm:"column:payment_intent_id;type:varchar(64)"                        json:"payment_intent_id,omitempty"`
	ChargeID        string                       `gorm:"column:charge_id;type:varchar(64)"                                json:"charge_id,omitempty"`
	RefundID        string                       `gorm:"column:refund_id;type:varchar(64)"                                json:"refund_id,omitempty"`
	DisputeID       string                       `gorm:"column:dispute_id;type:varchar(64)"                               json:"dispute_id,omitempty"`
	SignatureOK     bool                         `gorm:"column:signature_ok"                                              json:"signature_ok"`
	Headers         Metadata                     `gorm:"column:headers;type:json"                                         json:"headers,omitempty"`
	Body            []byte                       `gorm:"column:body;type:mediumblob"                                      json:"body,omitempty"`
	ProcessedAt     *time.Time                   `gorm:"column:processed_at"                                              json:"processed_at,omitempty"`
	ProcessStatus   InboundWebhookProcessStatus  `gorm:"column:process_status;type:varchar(16)"                           json:"process_status"`
	ErrorMsg        string                       `gorm:"column:error_msg;type:text"                                       json:"error_msg,omitempty"`
	Created         time.Time                    `gorm:"column:created"                                                   json:"created"`
}

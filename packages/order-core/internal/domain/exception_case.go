package domain

import (
	"errors"
	"time"
)

// ExceptionCaseType 差错类型
type ExceptionCaseType string

const (
	// ExceptionLateRefundSuccess 退款单已过期（status=expired），但渠道之后又回调"退款成功"
	ExceptionLateRefundSuccess ExceptionCaseType = "late_refund_success"
	// ExceptionLateRefundFailed 退款单已过期，渠道之后回调"退款失败"
	ExceptionLateRefundFailed ExceptionCaseType = "late_refund_failed"
	// ExceptionLateChargeSuccess 支付单已过期但渠道回调成功（触发自动补偿退款）
	ExceptionLateChargeSuccess ExceptionCaseType = "late_charge_success"
	// ExceptionAmountMismatch 渠道回传金额与单据不符
	ExceptionAmountMismatch ExceptionCaseType = "amount_mismatch"
	// ExceptionDuplicateWebhook 同一 event_id 多次回调
	ExceptionDuplicateWebhook ExceptionCaseType = "duplicate_webhook"
)

// ExceptionCaseStatus 差错单状态
type ExceptionCaseStatus string

const (
	ExceptionStatusOpen       ExceptionCaseStatus = "open"        // 待人工处理
	ExceptionStatusProcessing ExceptionCaseStatus = "processing"  // 处理中（系统自动或人工正在处理）
	ExceptionStatusResolved   ExceptionCaseStatus = "resolved"    // 已处理完毕
	ExceptionStatusIgnored    ExceptionCaseStatus = "ignored"     // 确认无需处理
)

// ErrExceptionCaseNotFound 差错单不存在
var ErrExceptionCaseNotFound = errors.New("exception_case not found")

// ExceptionCase 差错处理单（对应 exception_case_XX 分片表，按 pi 路由）。
//
// 典型场景：退款单已 expired / canceled 但渠道之后又返回结果——走差错处理流程，
// 人工介入或系统尝试自动修正，避免对账不平。
type ExceptionCase struct {
	ID              string              `gorm:"column:id;primaryKey;type:varchar(64)"                   json:"id"`
	PaymentIntentID string              `gorm:"column:payment_intent_id;type:varchar(64);index:idx_pi"  json:"payment_intent_id"`
	ChargeID        string              `gorm:"column:charge_id;type:varchar(64)"                       json:"charge_id,omitempty"`
	RefundID        string              `gorm:"column:refund_id;type:varchar(64);index:idx_refund"      json:"refund_id,omitempty"`
	CaseType        ExceptionCaseType   `gorm:"column:case_type;type:varchar(32);index:idx_type"        json:"case_type"`
	Status          ExceptionCaseStatus `gorm:"column:status;type:varchar(16);index:idx_status"         json:"status"`
	Summary         string              `gorm:"column:summary;type:varchar(512)"                        json:"summary"`
	// Payload 原始回调 / 对账差异明细（JSON）
	Payload Metadata `gorm:"column:payload;type:json" json:"payload,omitempty"`
	// Resolution 处理结论（人工填写或系统自动记录）
	Resolution string     `gorm:"column:resolution;type:varchar(512)" json:"resolution,omitempty"`
	AssignedTo string     `gorm:"column:assigned_to;type:varchar(64)" json:"assigned_to,omitempty"`
	ResolvedAt *time.Time `gorm:"column:resolved_at" json:"resolved_at,omitempty"`
	Created    time.Time  `gorm:"column:created" json:"created"`
	Updated    time.Time  `gorm:"column:updated" json:"updated"`
}

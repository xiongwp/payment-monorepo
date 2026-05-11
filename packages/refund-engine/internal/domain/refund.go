// Package domain — refund-engine 实体。
//
// 把退款逻辑从 order-core 拆出来独立服务，因为退款流程复杂:
//   - 部分退 / 全额退
//   - 退原通道 vs 退 wallet/余额
//   - fee 退不退（看 billing rule.refund_fee_behavior）
//   - 跨币种退（按原汇率还是按今天汇率？业务策略）
//   - 退款顺序（已结算 vs 未结算 资金来源不同）
//   - 退款后续：账户调整 / billing fee_event(refund) / merchant webhook
//
// 状态机:
//
//   requested → approved → submitted → completed / failed
//                ↓
//             rejected (ops 拒绝)

package domain

import "time"

// Refund 一笔退款请求。
type Refund struct {
	ID              int64        `db:"id" json:"id"`
	RefundID        string       `db:"refund_id" json:"refund_id"`               // re_xxx 业务 ID
	MerchantID      string       `db:"merchant_id" json:"merchant_id"`
	ChargeID        string       `db:"charge_id" json:"charge_id"`               // 原 charge ID
	PaymentIntentID string       `db:"payment_intent_id" json:"payment_intent_id"`
	AmountMinor     int64        `db:"amount_minor" json:"amount_minor"`         // 本次退款金额
	OriginalChargeAmountMinor int64 `db:"original_charge_amount_minor" json:"original_charge_amount_minor"`
	AlreadyRefundedMinor int64   `db:"already_refunded_minor" json:"already_refunded_minor"` // 同 charge 之前已退过的
	Currency        string       `db:"currency" json:"currency"`
	Reason          ReasonCode   `db:"reason" json:"reason"`
	ReasonNote      string       `db:"reason_note" json:"reason_note,omitempty"`
	Method          RefundMethod `db:"method" json:"method"`                     // original_channel / wallet / bank_transfer
	Status          Status       `db:"status" json:"status"`
	ChannelRefundID string       `db:"channel_refund_id" json:"channel_refund_id,omitempty"` // 通道回执
	FailureCode     string       `db:"failure_code" json:"failure_code,omitempty"`
	FailureMessage  string       `db:"failure_message" json:"failure_message,omitempty"`
	IdempotencyKey  string       `db:"idempotency_key" json:"idempotency_key"`
	RequestedBy     string       `db:"requested_by" json:"requested_by"`         // 'customer' / 'merchant' / 'ops' / 'chargeback'
	ApprovedBy      string       `db:"approved_by" json:"approved_by,omitempty"`
	RequestedAt     time.Time    `db:"requested_at" json:"requested_at"`
	ApprovedAt      *time.Time   `db:"approved_at" json:"approved_at,omitempty"`
	SubmittedAt     *time.Time   `db:"submitted_at" json:"submitted_at,omitempty"`
	CompletedAt     *time.Time   `db:"completed_at" json:"completed_at,omitempty"`
	TraceID         string       `db:"trace_id" json:"trace_id,omitempty"`
	CreatedAt       time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time    `db:"updated_at" json:"updated_at"`
}

// ReasonCode 退款原因 — 给商户后台分类统计 + 高占比预警风控。
type ReasonCode string

const (
	ReasonCustomerRequest ReasonCode = "customer_request" // 用户取消
	ReasonDuplicate       ReasonCode = "duplicate"        // 重复扣款
	ReasonFraudulent      ReasonCode = "fraudulent"       // 欺诈交易
	ReasonProductIssue    ReasonCode = "product_issue"    // 商品问题
	ReasonNotReceived     ReasonCode = "not_received"     // 商品未收到
	ReasonChargebackPrevent ReasonCode = "chargeback_prevent" // 主动退避免拒付
	ReasonOther           ReasonCode = "other"
)

// RefundMethod 退到哪里。
type RefundMethod string

const (
	MethodOriginalChannel RefundMethod = "original_channel" // 退回原卡/原通道（最常见）
	MethodWallet          RefundMethod = "wallet"           // 退商户/客户钱包余额
	MethodBankTransfer    RefundMethod = "bank_transfer"    // 银行转账（跨平台时用）
)

// Status 状态机。
type Status string

const (
	StatusRequested Status = "requested" // 入参刚到，等审核（大额）/ 直接 approved（小额）
	StatusApproved  Status = "approved"  // 已批准，等送通道
	StatusSubmitted Status = "submitted" // 已提交通道，等通道回执
	StatusCompleted Status = "completed" // 通道确认退款成功
	StatusFailed    Status = "failed"    // 通道拒绝 / 网络错
	StatusRejected  Status = "rejected"  // ops 审核拒绝（疑似欺诈退款等）
)

// IsTerminal 终态。
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusRejected
}

// ValidTransition 状态机白名单。
func ValidTransition(from, to Status) bool {
	if from.IsTerminal() {
		return false
	}
	switch from {
	case StatusRequested:
		return to == StatusApproved || to == StatusRejected
	case StatusApproved:
		return to == StatusSubmitted || to == StatusFailed
	case StatusSubmitted:
		return to == StatusCompleted || to == StatusFailed
	}
	return false
}

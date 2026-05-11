// Package domain — dispute (chargeback) 实体 + 状态机。
//
// Dispute (信用卡争议) 是支付系统合规必备 — 持卡人投诉 unauthorized / fraud /
// product not received 等，发卡行向收单行发起 chargeback。
//
// 流程时间线（典型 Visa/MC，地区有差）:
//
//   T+0    持卡人投诉 → 发卡行
//   T+1    发卡行通过卡组发 chargeback notification
//   T+1    收单行（我们）收到 webhook → 创建 Dispute (status=needs_response)
//   T+1    资金从商户余额 hold 走 (reserve)
//   T+...  商户提交证据（PDF / 收据 / 物流单 / 用户协议）
//   T+10~21 商户 deadline 内必须 response，否则自动 lost
//   T+30   卡组裁定 → status=won/lost
//
// 状态机:
//
//   needs_response (商户必须 N 天内 response)
//     │
//     ├── evidence_submitted   (商户提交了证据)
//     │     │
//     │     ├── won            (卡组裁定我方赢) → 资金回 merchant
//     │     ├── lost           (卡组裁定我方输) → 资金留给 cardholder
//     │     └── arbitration    (升级仲裁，少见)
//     │
//     └── expired              (商户未在 deadline 内提交) → 自动 lost

package domain

import "time"

// Dispute 一笔争议。
type Dispute struct {
	ID                int64         `db:"id" json:"id"`
	ExternalID        string        `db:"external_id" json:"external_id"`     // 卡组的 case_id
	MerchantID        string        `db:"merchant_id" json:"merchant_id"`
	ChargeID          string        `db:"charge_id" json:"charge_id"`         // 关联 payment-channel charge
	PiID              string        `db:"pi_id" json:"pi_id"`                 // 关联 order-core payment_intent
	AmountMinor       int64         `db:"amount_minor" json:"amount_minor"`
	Currency          string        `db:"currency" json:"currency"`
	Reason            ReasonCode    `db:"reason" json:"reason"`               // fraud / not_received / duplicate / ...
	Status            DisputeStatus `db:"status" json:"status"`
	NetworkCaseID     string        `db:"network_case_id" json:"network_case_id"`     // visa: ARN, mc: case ID
	Network           string        `db:"network" json:"network"`                     // visa / mastercard / amex / discover
	ReceivedAt        time.Time     `db:"received_at" json:"received_at"`             // 收到 chargeback 通知
	ResponseDeadline  time.Time     `db:"response_deadline" json:"response_deadline"` // 商户必须 response 的截止
	EvidenceSubmittedAt *time.Time  `db:"evidence_submitted_at" json:"evidence_submitted_at,omitempty"`
	RuledAt           *time.Time    `db:"ruled_at" json:"ruled_at,omitempty"`         // 卡组裁定时间
	RuledOutcome      string        `db:"ruled_outcome" json:"ruled_outcome,omitempty"` // won / lost
	NetworkFeeMinor   int64         `db:"network_fee_minor" json:"network_fee_minor"` // 卡组对接收单行收的 chargeback 处理费（~$15）
	TraceID           string        `db:"trace_id" json:"trace_id,omitempty"`
	CreatedAt         time.Time     `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time     `db:"updated_at" json:"updated_at"`
}

// ReasonCode 卡组 chargeback 原因（Visa/MC 各几十种，这里收最常见）。
type ReasonCode string

const (
	ReasonFraud        ReasonCode = "fraud"             // 未授权交易
	ReasonNotReceived  ReasonCode = "not_received"      // 物品/服务未收到
	ReasonDefective    ReasonCode = "defective"         // 商品有问题
	ReasonDuplicate    ReasonCode = "duplicate"         // 重复扣款
	ReasonCanceled     ReasonCode = "canceled_recurring" // 订阅未成功取消
	ReasonCredit       ReasonCode = "credit_not_received" // 退款未到
	ReasonAuthorization ReasonCode = "auth_error"       // 授权问题
	ReasonOther        ReasonCode = "other"
)

// DisputeStatus 状态机。
type DisputeStatus string

const (
	StatusNeedsResponse     DisputeStatus = "needs_response"
	StatusEvidenceSubmitted DisputeStatus = "evidence_submitted"
	StatusWon               DisputeStatus = "won"
	StatusLost              DisputeStatus = "lost"
	StatusExpired           DisputeStatus = "expired"   // 商户没在 deadline 提交，自动 lost
	StatusArbitration       DisputeStatus = "arbitration" // 升级仲裁
	StatusVoided            DisputeStatus = "voided"    // 卡组撤销 chargeback（罕见）
)

// IsTerminal 是否终态。
func (s DisputeStatus) IsTerminal() bool {
	return s == StatusWon || s == StatusLost || s == StatusExpired || s == StatusVoided
}

// Evidence 商户提交的证据 (单个 dispute 可能多个证据 item)。
type Evidence struct {
	ID             int64     `db:"id" json:"id"`
	DisputeID      int64     `db:"dispute_id" json:"dispute_id"`
	Type           string    `db:"type" json:"type"` // receipt / shipping_doc / customer_communication / refund_policy
	FileURL        string    `db:"file_url" json:"file_url"`         // S3 URL
	FileSize       int64     `db:"file_size" json:"file_size"`
	FileMime       string    `db:"file_mime" json:"file_mime"`
	Note           string    `db:"note" json:"note,omitempty"`
	SubmittedBy    string    `db:"submitted_by" json:"submitted_by"` // merchant email
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

// DisputeEvent 状态迁移事件（audit log）。
type DisputeEvent struct {
	ID         int64         `db:"id" json:"id"`
	DisputeID  int64         `db:"dispute_id" json:"dispute_id"`
	EventType  string        `db:"event_type" json:"event_type"` // received / evidence_submitted / ruled_won / ruled_lost / expired
	From       DisputeStatus `db:"from_status" json:"from_status"`
	To         DisputeStatus `db:"to_status" json:"to_status"`
	Actor      string        `db:"actor" json:"actor"`           // 'network' / 'merchant' / 'ops' / 'system'
	Details    string        `db:"details" json:"details,omitempty"`
	CreatedAt  time.Time     `db:"created_at" json:"created_at"`
}

// ValidTransition 状态机迁移合法性。
func ValidTransition(from, to DisputeStatus) bool {
	if from.IsTerminal() {
		return false
	}
	switch from {
	case StatusNeedsResponse:
		return to == StatusEvidenceSubmitted || to == StatusExpired ||
			to == StatusLost || to == StatusVoided
	case StatusEvidenceSubmitted:
		return to == StatusWon || to == StatusLost || to == StatusArbitration ||
			to == StatusVoided
	case StatusArbitration:
		return to == StatusWon || to == StatusLost
	}
	return false
}

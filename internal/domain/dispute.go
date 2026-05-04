package domain

import (
	"errors"
	"time"
)

// ErrDisputeNotFound dispute 不存在
var ErrDisputeNotFound = errors.New("dispute not found")

// ErrDisputeInvalidTransition dispute 状态机非法跃迁
var ErrDisputeInvalidTransition = errors.New("invalid dispute transition")

// DisputeStatus —— 渠道维度差异很大（卡组织 vs 电子钱包 vs 银行），
// 我们把 PH 场景能映射到的几个收敛成统一小集合。
type DisputeStatus string

const (
	DisputeNeedsResponse  DisputeStatus = "needs_response"  // 刚到，商户要交证据
	DisputeUnderReview    DisputeStatus = "under_review"    // 已交证据，渠道审核中
	DisputeWon            DisputeStatus = "won"             // 商户赢
	DisputeLost           DisputeStatus = "lost"            // 商户输；款项扣回
	DisputeWarningClosed  DisputeStatus = "warning_closed"  // 仅预警；无需响应
	DisputeChargeRefunded DisputeStatus = "charge_refunded" // 商户主动退款关闭
	DisputeCanceled       DisputeStatus = "canceled"        // 渠道撤回
)

// DisputeReason 归一化的原因集合（映射每家渠道自己的代码）。
type DisputeReason string

const (
	DisputeReasonFraud               DisputeReason = "fraud"
	DisputeReasonProductNotReceived  DisputeReason = "product_not_received"
	DisputeReasonUnrecognized        DisputeReason = "unrecognized"
	DisputeReasonDuplicate           DisputeReason = "duplicate"
	DisputeReasonCreditNotProcessed  DisputeReason = "credit_not_processed"
	DisputeReasonOther               DisputeReason = "other"
)

// disputeTransitions FSM 合法跃迁。rejected/terminated 是终态无出边。
var disputeTransitions = map[DisputeStatus]map[DisputeStatus]bool{
	DisputeNeedsResponse: {
		DisputeUnderReview:    true,
		DisputeWarningClosed:  true,
		DisputeChargeRefunded: true,
		DisputeCanceled:       true,
	},
	DisputeUnderReview: {
		DisputeWon:            true,
		DisputeLost:           true,
		DisputeNeedsResponse:  true, // 审核退回要求补材料
		DisputeChargeRefunded: true,
		DisputeCanceled:       true,
	},
	// Won / Lost / WarningClosed / ChargeRefunded / Canceled 是终态
}

// CanTransition 判断合法跃迁
func (s DisputeStatus) CanTransition(to DisputeStatus) bool {
	allowed, ok := disputeTransitions[s]
	if !ok {
		return false
	}
	return allowed[to]
}

// IsTerminal 是否终态
func (s DisputeStatus) IsTerminal() bool {
	switch s {
	case DisputeWon, DisputeLost, DisputeWarningClosed, DisputeChargeRefunded, DisputeCanceled:
		return true
	}
	return false
}

// Dispute 对应 dispute_XX 分片表（按 payment_intent_id 路由）。
type Dispute struct {
	ID               string        `gorm:"column:id;primaryKey;type:varchar(64)"         json:"id"`
	PaymentIntentID  string        `gorm:"column:payment_intent_id;type:varchar(64)"     json:"payment_intent_id"`
	ChargeID         string        `gorm:"column:charge_id;type:varchar(64)"             json:"charge_id"`
	MerchantID       string        `gorm:"column:merchant_id;type:varchar(32)"           json:"merchant_id"`
	Channel          string        `gorm:"column:channel;type:varchar(32)"               json:"channel"`
	ChannelDisputeID string        `gorm:"column:channel_dispute_id;type:varchar(128)"   json:"channel_dispute_id,omitempty"`
	Status           DisputeStatus `gorm:"column:status;type:varchar(24)"                json:"status"`
	Amount           int64         `gorm:"column:amount"                                 json:"amount"`
	Currency         string        `gorm:"column:currency;type:char(3)"                  json:"currency"`
	Reason           DisputeReason `gorm:"column:reason;type:varchar(64)"                json:"reason"`
	ReasonDetail     string        `gorm:"column:reason_detail;type:varchar(512)"        json:"reason_detail,omitempty"`
	EvidenceDueAt    *time.Time    `gorm:"column:evidence_due_at"                        json:"evidence_due_at,omitempty"`
	Evidence         Metadata      `gorm:"column:evidence;type:json"                     json:"evidence,omitempty"`
	DecidedAt        *time.Time    `gorm:"column:decided_at"                             json:"decided_at,omitempty"`
	OutcomeAmount    *int64        `gorm:"column:outcome_amount"                         json:"outcome_amount,omitempty"`
	AutoRefundCharge bool          `gorm:"column:auto_refund_charge"                     json:"auto_refund_charge"`
	Metadata         Metadata      `gorm:"column:metadata;type:json"                     json:"metadata,omitempty"`
	Created          time.Time     `gorm:"column:created"                                json:"created"`
	Updated          time.Time     `gorm:"column:updated"                                json:"updated"`
}

// DisputeEvent 状态流转日志（append-only）
type DisputeEvent struct {
	ID              int64     `gorm:"column:id;primaryKey;autoIncrement"        json:"id"`
	DisputeID       string    `gorm:"column:dispute_id;type:varchar(64)"        json:"dispute_id"`
	PaymentIntentID string    `gorm:"column:payment_intent_id;type:varchar(64)" json:"payment_intent_id"`
	FromStatus      string    `gorm:"column:from_status;type:varchar(24)"       json:"from_status"`
	ToStatus        string    `gorm:"column:to_status;type:varchar(24)"         json:"to_status"`
	Source          string    `gorm:"column:source;type:varchar(32)"            json:"source"` // channel_webhook / admin / system
	Actor           string    `gorm:"column:actor;type:varchar(64)"             json:"actor,omitempty"`
	Note            string    `gorm:"column:note;type:varchar(512)"             json:"note,omitempty"`
	Payload         Metadata  `gorm:"column:payload;type:json"                  json:"payload,omitempty"`
	Created         time.Time `gorm:"column:created"                            json:"created"`
}

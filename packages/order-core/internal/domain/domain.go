// Package domain 定义 Stripe 风格的核心实体：PaymentIntent / Charge / Refund。
package domain

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// ─── 通用 ─────────────────────────────────────────────────────────────────────

// Metadata 字符串到字符串的 map，序列化为 JSON 列。
type Metadata map[string]string

// Value implements driver.Valuer
func (m Metadata) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// Scan implements sql.Scanner
func (m *Metadata) Scan(src any) error {
	if src == nil {
		*m = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return errors.New("domain: unsupported Scan type for Metadata")
	}
	if len(data) == 0 {
		*m = nil
		return nil
	}
	return json.Unmarshal(data, m)
}

// StringList 字符串切片，序列化为 JSON 列
type StringList []string

// Value implements driver.Valuer
func (l StringList) Value() (driver.Value, error) {
	if l == nil {
		return nil, nil
	}
	return json.Marshal(l)
}

// Scan implements sql.Scanner
func (l *StringList) Scan(src any) error {
	if src == nil {
		*l = nil
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return errors.New("domain: unsupported Scan type for StringList")
	}
	if len(data) == 0 {
		*l = nil
		return nil
	}
	return json.Unmarshal(data, l)
}

// ─── 错误 ─────────────────────────────────────────────────────────────────────

// ErrPaymentIntentNotFound PaymentIntent 不存在
var ErrPaymentIntentNotFound = errors.New("payment_intent not found")

// ErrChargeNotFound Charge 不存在
var ErrChargeNotFound = errors.New("charge not found")

// ErrRefundNotFound Refund 不存在
var ErrRefundNotFound = errors.New("refund not found")

// ErrInvalidTransition 状态机非法迁移
var ErrInvalidTransition = errors.New("invalid status transition")

// ErrValidation 业务校验失败
var ErrValidation = errors.New("validation failed")

// ErrRefundAmountExceeded 退款金额超出可退余额
var ErrRefundAmountExceeded = errors.New("refund amount exceeds available")

// ─── 枚举 ─────────────────────────────────────────────────────────────────────

// PaymentIntentStatus 状态机（沿用先前 6 态版本，对外按 Stripe 风格小写命名）
type PaymentIntentStatus string

const (
	PIStatusCreated        PaymentIntentStatus = "created"
	PIStatusRequiresAction PaymentIntentStatus = "requires_action"
	PIStatusProcessing     PaymentIntentStatus = "processing"
	PIStatusSucceeded      PaymentIntentStatus = "succeeded"
	PIStatusFailed         PaymentIntentStatus = "failed"
	PIStatusCanceled       PaymentIntentStatus = "canceled"
)

// RefundPhase 订单退款维度的聚合状态（独立于 PaymentIntentStatus）。
// 只有在 PI.Status == SUCCEEDED 且有 Refund 活动时才有意义。
type RefundPhase string

const (
	RefundPhaseNone              RefundPhase = ""                   // 未发起过退款
	RefundPhaseRefunding         RefundPhase = "refunding"          // 至少一笔退款进行中
	RefundPhasePartiallyRefunded RefundPhase = "partially_refunded" // 部分金额已退，仍可继续退
	RefundPhaseFullyRefunded     RefundPhase = "fully_refunded"     // 全部已退（退款维度终态）
	RefundPhaseRefundFailed      RefundPhase = "refund_failed"      // 最近一笔退款失败，用户可再次发起
)

// RefundPhaseTransitions 退款维度状态机
//
//   NONE                ──发起退款──→ REFUNDING
//   REFUNDING           → PARTIALLY_REFUNDED / FULLY_REFUNDED / REFUND_FAILED / REFUND_CANCELED
//   PARTIALLY_REFUNDED  ──再次发起──→ REFUNDING
//   REFUND_FAILED       ──重试──→    REFUNDING
//   REFUND_CANCELED     → NONE / PARTIALLY_REFUNDED / REFUNDING
//   FULLY_REFUNDED      终态
var RefundPhaseTransitions = map[RefundPhase]map[RefundPhase]struct{}{
	RefundPhaseNone:              {RefundPhaseRefunding: {}},
	RefundPhaseRefunding:         {RefundPhasePartiallyRefunded: {}, RefundPhaseFullyRefunded: {}, RefundPhaseRefundFailed: {}},
	RefundPhasePartiallyRefunded: {RefundPhaseRefunding: {}},
	RefundPhaseRefundFailed:      {RefundPhaseRefunding: {}},
	RefundPhaseFullyRefunded:     {}, // 终态
}

// CanRefundPhaseTransition 判断退款阶段状态迁移是否合法
func CanRefundPhaseTransition(from, to RefundPhase) bool {
	if from == to {
		return false
	}
	allowed, ok := RefundPhaseTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// CaptureMethod 捕获方式
type CaptureMethod string

const (
	CaptureAutomatic CaptureMethod = "automatic"
	CaptureManual    CaptureMethod = "manual"
)

// ConfirmationMethod 确认方式
type ConfirmationMethod string

const (
	ConfirmationAutomatic ConfirmationMethod = "automatic"
	ConfirmationManual    ConfirmationMethod = "manual"
)

// ChargeStatus Stripe Charge 状态
type ChargeStatus string

const (
	ChargeStatusPending   ChargeStatus = "pending"
	ChargeStatusSucceeded ChargeStatus = "succeeded"
	ChargeStatusFailed    ChargeStatus = "failed"
	ChargeStatusExpired   ChargeStatus = "expired" // 超时未完成；终态不可再推进
)

// RefundStatus Stripe Refund 状态
type RefundStatus string

const (
	RefundStatusPending   RefundStatus = "pending"
	RefundStatusSucceeded RefundStatus = "succeeded"
	RefundStatusFailed    RefundStatus = "failed"
)
// 退款单状态机极简化 — 只允许 pending → succeeded / failed。
//
// 原因：允许 canceled / expired 等中间终态会给对账留下"既没退钱又标记终结"的窟窿，
// 容易出现资损（用户没收到退款但系统已经关单）。
//
// 后续处理：
//   - 商户主动发起的 Refund 如果 failed：商户可以再创建新单重试
//   - 系统补偿 Refund（auto_compensate=1）：retry worker 无限重试直到 succeeded

// RefundReason 退款原因
type RefundReason string

const (
	RefundReasonDuplicate            RefundReason = "duplicate"
	RefundReasonFraudulent           RefundReason = "fraudulent"
	RefundReasonRequestedByCustomer  RefundReason = "requested_by_customer"
	RefundReasonExpiredUncaptured    RefundReason = "expired_uncaptured_charge"
)

// ─── PaymentIntent ────────────────────────────────────────────────────────────

// PaymentIntent 对应 payment_intent_XX 分片表
type PaymentIntent struct {
	ID                  string              `gorm:"column:id;primaryKey;type:varchar(64)"           json:"id"`
	Amount              int64               `gorm:"column:amount"                                   json:"amount"` // 应付金额 = subtotal - coupon - points
	// 金额分解（可选；前端拆单展示用。refund 仍以 charge.amount_captured 为准）
	AmountSubtotal int64 `gorm:"column:amount_subtotal" json:"amount_subtotal,omitempty"`
	AmountCoupon   int64 `gorm:"column:amount_coupon"   json:"amount_coupon,omitempty"`
	AmountPoints   int64 `gorm:"column:amount_points"   json:"amount_points,omitempty"`
	Currency            string              `gorm:"column:currency;type:char(3)"                    json:"currency"`
	Status              PaymentIntentStatus `gorm:"column:status;type:varchar(32);index:idx_status" json:"status"`
	// RefundPhase 退款维度的状态；仅在 Status=succeeded 后有意义。见 RefundPhaseTransitions。
	RefundPhase         RefundPhase         `gorm:"column:refund_phase;type:varchar(32);index:idx_refund_phase" json:"refund_phase,omitempty"`
	CustomerID          string              `gorm:"column:customer_id;type:varchar(64);index:idx_customer" json:"customer_id,omitempty"`
	Description         string              `gorm:"column:description;type:varchar(512)"            json:"description,omitempty"`
	MchID               string              `gorm:"column:mch_id;type:varchar(32);index:idx_mch;uniqueIndex:uk_mch_idem,priority:1" json:"mch_id"`
	MchOrderNo          string              `gorm:"column:mch_order_no;type:varchar(64)"            json:"mch_order_no,omitempty"`
	// BusinessID 分片路由键。pi_id 的分片前缀由 business_id 哈希决定，
	// 这样"按 business_id 查"和"按 pi_id 查"落在同一个分片。为空时 fallback 到 mch_id。
	BusinessID          string              `gorm:"column:business_id;type:varchar(64);index:idx_business" json:"business_id,omitempty"`
	// IdempotencyKey 幂等键：(mch_id, idempotency_key) 唯一。
	// 相同 key 的重复 Create 请求返回首次创建的结果，不会生成新 PI。
	IdempotencyKey      string              `gorm:"column:idempotency_key;type:varchar(128);uniqueIndex:uk_mch_idem,priority:2" json:"idempotency_key,omitempty"`
	// PreviousPaymentIntentID 指向上一次失败的 PI（换单重新下单场景）。
	PreviousPaymentIntentID string          `gorm:"column:previous_payment_intent_id;type:varchar(64);index:idx_prev" json:"previous_payment_intent_id,omitempty"`
	CaptureMethod       CaptureMethod       `gorm:"column:capture_method;type:varchar(16)"          json:"capture_method"`
	ConfirmationMethod  ConfirmationMethod  `gorm:"column:confirmation_method;type:varchar(16)"     json:"confirmation_method"`
	ClientSecret        string              `gorm:"column:client_secret;type:varchar(128)"          json:"client_secret,omitempty"`
	PaymentMethodTypes  StringList          `gorm:"column:payment_method_types;type:json"           json:"payment_method_types,omitempty"`
	PaymentMethod       string              `gorm:"column:payment_method;type:varchar(32)"          json:"payment_method,omitempty"`
	// UserID 付款用户 id（可空，匿名 PI / 商户后台代下单时为 0）。卡支付路径必填。
	UserID              int64               `gorm:"column:user_id"                                  json:"user_id,omitempty"`
	// UserCardID 卡支付时关联 user-merchant.user_card.id；其它支付方式为 0。
	// 不为 0 时 Confirm 路径会调 card-center.CreatePaymentToken 派生支付 token。
	UserCardID          int64               `gorm:"column:user_card_id"                             json:"user_card_id,omitempty"`
	// ActiveChargeIDs / ActiveRefundIDs 只存"正在进行中"的支付/退款单 id。
	// Charge / Refund 到达终态（succeeded/failed/canceled）后由服务层从列表中移除，
	// 历史明细以 charge_XX / refund_XX 表为准。
	ActiveChargeIDs StringList `gorm:"column:active_charge_ids;type:json" json:"active_charge_ids,omitempty"`
	ActiveRefundIDs StringList `gorm:"column:active_refund_ids;type:json" json:"active_refund_ids,omitempty"`
	AmountCapturable    int64               `gorm:"column:amount_capturable"                        json:"amount_capturable"`
	AmountReceived      int64               `gorm:"column:amount_received"                          json:"amount_received"`
	ReturnURL           string              `gorm:"column:return_url;type:varchar(512)"             json:"return_url,omitempty"`
	NotifyURL           string              `gorm:"column:notify_url;type:varchar(512)"             json:"notify_url,omitempty"`
	StatementDescriptor string              `gorm:"column:statement_descriptor;type:varchar(32)"    json:"statement_descriptor,omitempty"`
	Metadata            Metadata            `gorm:"column:metadata;type:json"                       json:"metadata,omitempty"`
	Livemode            bool                `gorm:"column:livemode"                                 json:"livemode"`
	Created             time.Time           `gorm:"column:created"                                  json:"created"`
	Updated             time.Time           `gorm:"column:updated"                                  json:"updated"`
	ExpiredAt           *time.Time          `gorm:"column:expired_at;index:idx_expired_at"          json:"expired_at,omitempty"`
	CanceledAt          *time.Time          `gorm:"column:canceled_at"                              json:"canceled_at,omitempty"`
	CancellationReason  string              `gorm:"column:cancellation_reason;type:varchar(32)"     json:"cancellation_reason,omitempty"`
}

// PIValidTransitions PaymentIntent 状态机（只管支付生命周期，退款走 RefundPhase 字段）
//
//   CREATED         → REQUIRES_ACTION / PROCESSING / CANCELED
//   REQUIRES_ACTION → PROCESSING / REQUIRES_ACTION(自环) / CANCELED
//   PROCESSING      → SUCCEEDED / FAILED
//   SUCCEEDED / FAILED / CANCELED 为终态
//
// 退款维度请见 RefundPhaseTransitions。
var PIValidTransitions = map[PaymentIntentStatus]map[PaymentIntentStatus]struct{}{
	PIStatusCreated: {
		PIStatusRequiresAction: {},
		PIStatusProcessing:     {},
		PIStatusCanceled:       {},
	},
	PIStatusRequiresAction: {
		PIStatusProcessing:     {},
		PIStatusRequiresAction: {},
		PIStatusCanceled:       {},
	},
	PIStatusProcessing: {
		PIStatusSucceeded: {},
		PIStatusFailed:    {},
	},
	PIStatusSucceeded: {},
	PIStatusFailed:    {},
	PIStatusCanceled:  {},
}

// CanTransition 判断 from → to 是否合法
func CanTransition(from, to PaymentIntentStatus) bool {
	if from == to {
		// 仅 REQUIRES_ACTION 自环允许
		return from == PIStatusRequiresAction
	}
	allowed, ok := PIValidTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// ─── Charge ───────────────────────────────────────────────────────────────────

// Charge 对应 charge_XX 分片表（与 PaymentIntent 同分片）
type Charge struct {
	ID                 string       `gorm:"column:id;primaryKey;type:varchar(64)"               json:"id"`
	PaymentIntentID    string       `gorm:"column:payment_intent_id;type:varchar(64);index:idx_pi" json:"payment_intent_id"`
	Amount             int64        `gorm:"column:amount"                                       json:"amount"`
	AmountCaptured     int64        `gorm:"column:amount_captured"                              json:"amount_captured"`
	AmountRefunded     int64        `gorm:"column:amount_refunded"                              json:"amount_refunded"`
	Currency           string       `gorm:"column:currency;type:char(3)"                        json:"currency"`
	Status             ChargeStatus `gorm:"column:status;type:varchar(16);index:idx_status"     json:"status"`
	PaymentMethod      string       `gorm:"column:payment_method;type:varchar(32)"              json:"payment_method"`
	Captured           bool         `gorm:"column:captured"                                     json:"captured"`
	Paid               bool         `gorm:"column:paid"                                         json:"paid"`
	Refunded           bool         `gorm:"column:refunded"                                     json:"refunded"`
	OutcomeRiskLevel   string       `gorm:"column:outcome_risk_level;type:varchar(16)"          json:"outcome_risk_level,omitempty"`
	OutcomeRiskScore   int          `gorm:"column:outcome_risk_score"                           json:"outcome_risk_score,omitempty"`
	OutcomeSellerMsg   string       `gorm:"column:outcome_seller_msg;type:varchar(256)"         json:"outcome_seller_msg,omitempty"`
	OutcomeType        string       `gorm:"column:outcome_type;type:varchar(32)"                json:"outcome_type,omitempty"`
	OutcomeReason      string       `gorm:"column:outcome_reason;type:varchar(64)"              json:"outcome_reason,omitempty"`
	OutcomeNetwork     string       `gorm:"column:outcome_network;type:varchar(32)"             json:"outcome_network,omitempty"`
	FailureCode        string       `gorm:"column:failure_code;type:varchar(64)"                json:"failure_code,omitempty"`
	FailureMessage     string       `gorm:"column:failure_message;type:varchar(512)"            json:"failure_message,omitempty"`
	ReceiptURL         string       `gorm:"column:receipt_url;type:varchar(512)"                json:"receipt_url,omitempty"`
	BalanceTransaction string       `gorm:"column:balance_transaction;type:varchar(64)"         json:"balance_transaction,omitempty"`
	Livemode           bool         `gorm:"column:livemode"                                     json:"livemode"`
	Metadata           Metadata     `gorm:"column:metadata;type:json"                           json:"metadata,omitempty"`
	// ExpiredAt 支付单过期时间；cron 扫到未终态的超时支付 → 置 failed
	ExpiredAt          *time.Time   `gorm:"column:expired_at;index:idx_expired_at"              json:"expired_at,omitempty"`
	CompletedAt        *time.Time   `gorm:"column:completed_at"                                 json:"completed_at,omitempty"`
	Created            time.Time    `gorm:"column:created"                                      json:"created"`
	Updated            time.Time    `gorm:"column:updated"                                      json:"updated"`
}

// ─── Refund ───────────────────────────────────────────────────────────────────

// Refund 对应 refund_XX 分片表（与 PaymentIntent 同分片）
type Refund struct {
	ID              string       `gorm:"column:id;primaryKey;type:varchar(64)"                json:"id"`
	ChargeID        string       `gorm:"column:charge_id;type:varchar(64);index:idx_charge"   json:"charge_id"`
	PaymentIntentID string       `gorm:"column:payment_intent_id;type:varchar(64);index:idx_pi" json:"payment_intent_id"`
	Amount          int64        `gorm:"column:amount"                                        json:"amount"`
	Currency        string       `gorm:"column:currency;type:char(3)"                         json:"currency"`
	Status          RefundStatus `gorm:"column:status;type:varchar(16);index:idx_status"      json:"status"`
	Reason          RefundReason `gorm:"column:reason;type:varchar(32)"                       json:"reason,omitempty"`
	FailureReason   string       `gorm:"column:failure_reason;type:varchar(256)"              json:"failure_reason,omitempty"`
	ReceiptNumber   string       `gorm:"column:receipt_number;type:varchar(64)"               json:"receipt_number,omitempty"`
	Metadata        Metadata     `gorm:"column:metadata;type:json"                            json:"metadata,omitempty"`
	// AutoCompensate 系统自动补偿退款（晚到支付成功场景），retry worker 无限重试直到 succeeded
	AutoCompensate bool `gorm:"column:auto_compensate;index:idx_auto_compensate" json:"auto_compensate"`
	// RetryCount / NextRetryAt retry worker 状态字段
	RetryCount  int        `gorm:"column:retry_count"                        json:"retry_count"`
	NextRetryAt *time.Time `gorm:"column:next_retry_at;index:idx_next_retry" json:"next_retry_at,omitempty"`
	CompletedAt *time.Time `gorm:"column:completed_at"                       json:"completed_at,omitempty"`
	Created         time.Time    `gorm:"column:created"                                       json:"created"`
	Updated         time.Time    `gorm:"column:updated"                                       json:"updated"`
}

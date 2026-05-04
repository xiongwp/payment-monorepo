package domain

import (
	"errors"
	"time"
)

// ─── PayAction ────────────────────────────────────────────────────────────────

// PayActionType 支付过程中需要用户完成的"一件事"的类型（先实现三种）
type PayActionType string

const (
	// PayActionThreeDS 3D Secure 银行挑战（跳转 ACS 页面）
	PayActionThreeDS PayActionType = "three_d_secure"
	// PayActionOTP 短信 / 邮件 / 语音一次性验证码
	PayActionOTP PayActionType = "otp"
	// PayActionPayPassword 支付密码（哈希比对）
	PayActionPayPassword PayActionType = "pay_password"
)

// PayActionStatus PayAction 状态（简单 4 态，无链式）
type PayActionStatus string

const (
	PayActionStatusPending   PayActionStatus = "pending"
	PayActionStatusSucceeded PayActionStatus = "succeeded"
	PayActionStatusFailed    PayActionStatus = "failed"
	PayActionStatusExpired   PayActionStatus = "expired"
)

// ErrPayActionNotFound PayAction 不存在
var ErrPayActionNotFound = errors.New("pay_action not found")

// ErrPayActionNotPending 非 pending 状态不能提交 / 超时
var ErrPayActionNotPending = errors.New("pay_action is not pending")

// ErrPayActionTooManyAttempts 尝试次数耗尽
var ErrPayActionTooManyAttempts = errors.New("pay_action max attempts reached")

// PayAction 对应 pay_action_XX 分片表（与 PaymentIntent 同分片）。
//
// 一个 PaymentIntent 在处于 REQUIRES_ACTION 时，最多挂一个 pending 的 PayAction；
// 用户提交结果后，action 流转到 succeeded / failed / expired，PI 相应推进。
type PayAction struct {
	ID              string          `gorm:"column:id;primaryKey;type:varchar(64)"               json:"id"`
	PaymentIntentID string          `gorm:"column:payment_intent_id;type:varchar(64);index:idx_pi" json:"payment_intent_id"`
	ChargeID        string          `gorm:"column:charge_id;type:varchar(64);index:idx_charge" json:"charge_id,omitempty"`
	ActionType      PayActionType   `gorm:"column:action_type;type:varchar(32);index:idx_type" json:"action_type"`
	Status          PayActionStatus `gorm:"column:status;type:varchar(16);index:idx_status"    json:"status"`

	// Payload 渠道返回给前端的挑战材料：
	//   3D Secure → {redirect_url, return_url}
	//   OTP       → {recipient_masked, channel}   （内部 expected_otp 存在单独字段）
	//   PayPassword → {algorithm, salt?}           （expected_hash 在单独字段）
	Payload Metadata `gorm:"column:payload;type:json" json:"payload,omitempty"`

	// ExpectedSecret 服务端期望值（OTP 明文 / 密码哈希）。不返回给前端。
	ExpectedSecret string `gorm:"column:expected_secret;type:varchar(256)" json:"-"`

	// AttemptCount 用户已尝试次数（OTP / 密码重试计数）
	AttemptCount int `gorm:"column:attempt_count" json:"attempt_count"`
	// MaxAttempts 最大尝试次数，0 表示不限
	MaxAttempts int `gorm:"column:max_attempts" json:"max_attempts"`

	FailureReason string     `gorm:"column:failure_reason;type:varchar(256)" json:"failure_reason,omitempty"`
	Metadata      Metadata   `gorm:"column:metadata;type:json"               json:"metadata,omitempty"`
	ExpiresAt     *time.Time `gorm:"column:expires_at;index:idx_expires_at"  json:"expires_at,omitempty"`
	CompletedAt   *time.Time `gorm:"column:completed_at"                     json:"completed_at,omitempty"`
	Created       time.Time  `gorm:"column:created"                          json:"created"`
	Updated       time.Time  `gorm:"column:updated"                          json:"updated"`
}

// ValidActionTransitions 状态机（无链式，pending 终态化为 succeeded / failed / expired）
var ValidActionTransitions = map[PayActionStatus]map[PayActionStatus]struct{}{
	PayActionStatusPending: {
		PayActionStatusSucceeded: {},
		PayActionStatusFailed:    {},
		PayActionStatusExpired:   {},
	},
	PayActionStatusSucceeded: {},
	PayActionStatusFailed:    {},
	PayActionStatusExpired:   {},
}

// CanActionTransition action 状态合法性
func CanActionTransition(from, to PayActionStatus) bool {
	if from == to {
		return false
	}
	allowed, ok := ValidActionTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

// Package domain — Subscription 领域类型。
package domain

import "time"

// Plan 订阅计划 (商户后台一次配, N 个 subscription 共用)。
type Plan struct {
	ID              int64     `db:"id" json:"id"`
	Key             string    `db:"plan_key" json:"key"`          // 业务键, e.g. "pro_monthly"
	MerchantID      string    `db:"merchant_id" json:"merchant_id"`
	Name            string    `db:"name" json:"name"`
	AmountMinor     int64     `db:"amount_minor" json:"amount_minor"`
	Currency        string    `db:"currency" json:"currency"`
	IntervalUnit    string    `db:"interval_unit" json:"interval_unit"`     // day / week / month / year
	IntervalCount   int       `db:"interval_count" json:"interval_count"`   // 每 N 个 unit (e.g. 3 month)
	TrialDays       int       `db:"trial_days" json:"trial_days"`           // 试用期
	GracePeriodDays int       `db:"grace_period_days" json:"grace_period_days"` // dunning 期总长
	MaxCycles       int       `db:"max_cycles" json:"max_cycles"`           // 0 = 无限; e.g. 12 = 限 1 年
	MoneyFlowGraph  string    `db:"moneyflow_graph_key" json:"moneyflow_graph_key"` // 触发的 graph key
	Status          string    `db:"status" json:"status"`                   // active / archived
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

// Subscription 一个具体的订阅实例。
type Subscription struct {
	ID                  int64      `db:"id" json:"id"`
	SubscriptionID      string     `db:"subscription_id" json:"subscription_id"` // sub_xxx
	PlanID              int64      `db:"plan_id" json:"plan_id"`
	MerchantID          string     `db:"merchant_id" json:"merchant_id"`
	CustomerID          string     `db:"customer_id" json:"customer_id"`
	PaymentMethodID     string     `db:"payment_method_id" json:"payment_method_id"` // card token (从 card-center 拿)
	Status              string     `db:"status" json:"status"`                       // incomplete/trialing/active/past_due/canceled/ended
	CurrentPeriodStart  time.Time  `db:"current_period_start" json:"current_period_start"`
	CurrentPeriodEnd    time.Time  `db:"current_period_end" json:"current_period_end"`
	TrialEnd            *time.Time `db:"trial_end" json:"trial_end,omitempty"`
	CycleCount          int        `db:"cycle_count" json:"cycle_count"`             // 已扣款次数
	CancelAtPeriodEnd   bool       `db:"cancel_at_period_end" json:"cancel_at_period_end"`
	CanceledAt          *time.Time `db:"canceled_at" json:"canceled_at,omitempty"`
	DunningRetryCount   int        `db:"dunning_retry_count" json:"dunning_retry_count"`
	LastDunningAt       *time.Time `db:"last_dunning_at" json:"last_dunning_at,omitempty"`
	Attributes          map[string]string `db:"-" json:"attributes,omitempty"` // moneyflow graph 用 (seller_id 等)
	CreatedAt           time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt           time.Time  `db:"updated_at" json:"updated_at"`
}

// Invoice 一次周期的账单 (扣款前生成)。
type Invoice struct {
	ID             int64      `db:"id" json:"id"`
	InvoiceID      string     `db:"invoice_id" json:"invoice_id"` // inv_xxx
	SubscriptionID string     `db:"subscription_id" json:"subscription_id"`
	MerchantID     string     `db:"merchant_id" json:"merchant_id"`
	CustomerID     string     `db:"customer_id" json:"customer_id"`
	AmountMinor    int64      `db:"amount_minor" json:"amount_minor"`
	Currency       string     `db:"currency" json:"currency"`
	PeriodStart    time.Time  `db:"period_start" json:"period_start"`
	PeriodEnd      time.Time  `db:"period_end" json:"period_end"`
	Status         string     `db:"status" json:"status"`        // draft / open / paid / uncollectible / void
	ChargeID       string     `db:"charge_id" json:"charge_id,omitempty"` // 关联的 charge
	AttemptCount   int        `db:"attempt_count" json:"attempt_count"`
	NextRetryAt    *time.Time `db:"next_retry_at" json:"next_retry_at,omitempty"`
	PaidAt         *time.Time `db:"paid_at" json:"paid_at,omitempty"`
	FailureCode    string     `db:"failure_code" json:"failure_code,omitempty"`
	FailureMessage string     `db:"failure_message" json:"failure_message,omitempty"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
}

// DunningEvent 一次 dunning 重试记录 (审计用)。
type DunningEvent struct {
	ID             int64     `db:"id" json:"id"`
	SubscriptionID string    `db:"subscription_id" json:"subscription_id"`
	InvoiceID      string    `db:"invoice_id" json:"invoice_id"`
	Attempt        int       `db:"attempt" json:"attempt"`
	Action         string    `db:"action" json:"action"` // retry / email / sms / final_warning / cancel
	Outcome        string    `db:"outcome" json:"outcome"` // succeeded / failed / pending
	Detail         string    `db:"detail" json:"detail,omitempty"`
	OccurredAt     time.Time `db:"occurred_at" json:"occurred_at"`
}

// Constants
const (
	StatusIncomplete = "incomplete"
	StatusTrialing   = "trialing"
	StatusActive     = "active"
	StatusPastDue    = "past_due"
	StatusCanceled   = "canceled"
	StatusEnded      = "ended"
	StatusUnpaid     = "unpaid"

	InvoiceDraft       = "draft"
	InvoiceOpen        = "open"
	InvoicePaid        = "paid"
	InvoiceUncollect   = "uncollectible"
	InvoiceVoid        = "void"
)

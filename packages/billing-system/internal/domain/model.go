// Package domain — billing-system 核心实体。
//
// 5 个核心概念:
//
//	FeeRule       一条费率规则（按 merchant/product/channel/region/卡组 分级匹配）
//	FeeEvent      一笔交易触发的费用事件（每个 charge / refund 对应 0~N 条）
//	Statement     账单（按 merchant + period 聚合 fee_events）
//	Adjustment    手工调整（运营给 rebate / penalty 等）
//	Payout        商户提现请求（结算给真实银行账户）
//
// FeeRule 求值优先级（高 → 低）：
//
//	1. merchant_id 精确匹配
//	2. merchant_tier (gold/silver/...)
//	3. product (card_charge / wallet / bank_transfer / ...)
//	4. channel_adapter (visa/mc/gcash/...)
//	5. region (PH/SG/TW/...)
//	6. default fallback
//
// 每条 rule 写明 percent_bps + fixed_minor + min_minor + max_minor + currency 范围。

package domain

import (
	"time"
)

// FeeRule 一条费率规则。
//
// 数据库存：billing_db_<shard>.fee_rule
// 配置存：config-center key=billing/fee_rules（YAML/JSON）
//
// 命中策略：按 priority desc 找第一个匹配的（详见 feerule.Engine.Pick）。
type FeeRule struct {
	ID                int64   `json:"id" db:"id"`
	Name              string  `json:"name" db:"name"`             // human-readable，如 "Gold-Visa-PH-Default"
	Priority          int     `json:"priority" db:"priority"`     // 越大越优先
	MerchantID        string  `json:"merchant_id,omitempty" db:"merchant_id"` // 空 = 不限
	MerchantTier      string  `json:"merchant_tier,omitempty" db:"merchant_tier"`
	Product           string  `json:"product,omitempty" db:"product"`         // card_charge / wallet / ...
	ChannelAdapter    string  `json:"channel_adapter,omitempty" db:"channel_adapter"`
	Region            string  `json:"region,omitempty" db:"region"`           // ISO-3166 alpha-2
	CurrencyAllow     string  `json:"currency_allow,omitempty" db:"currency_allow"` // "USD,PHP" 逗号分隔；空 = 不限
	CardBINRange      string  `json:"card_bin_range,omitempty" db:"card_bin_range"` // "4000-4999" 等
	AmountMinMinor    int64   `json:"amount_min_minor" db:"amount_min_minor"`
	AmountMaxMinor    int64   `json:"amount_max_minor" db:"amount_max_minor"` // 0 = 不限
	PercentBPS        int     `json:"percent_bps" db:"percent_bps"`           // basis points (1% = 100bps)
	FixedMinor        int64   `json:"fixed_minor" db:"fixed_minor"`           // 固定费用 cents/centavos
	FeeMinMinor       int64   `json:"fee_min_minor" db:"fee_min_minor"`       // 单笔最低 fee
	FeeMaxMinor       int64   `json:"fee_max_minor" db:"fee_max_minor"`       // 单笔最高 fee（0 = 不限）
	FXMarkupBPS       int     `json:"fx_markup_bps,omitempty" db:"fx_markup_bps"` // 跨币种额外 markup
	RefundFeeBehavior string  `json:"refund_fee_behavior" db:"refund_fee_behavior"` // "refund"|"keep"|"prorate"
	EffectiveFrom     time.Time `json:"effective_from" db:"effective_from"`
	EffectiveTo       *time.Time `json:"effective_to,omitempty" db:"effective_to"`
	Active            bool    `json:"active" db:"active"`
	CreatedAt         time.Time `json:"created_at" db:"created_at"`
	UpdatedAt         time.Time `json:"updated_at" db:"updated_at"`
}

// FeeEvent 一笔交易事件触发的费用记录。
//
// 数据库存：billing_db_<shard>.fee_event_<table_idx>（按 merchant_id hash 分片分表）
type FeeEvent struct {
	ID              int64     `json:"id" db:"id"`
	MerchantID      string    `json:"merchant_id" db:"merchant_id"`
	EventType       EventType `json:"event_type" db:"event_type"`       // charge / refund / chargeback / adjustment
	RefID           string    `json:"ref_id" db:"ref_id"`               // pi_id / charge_id / refund_id
	RefService      string    `json:"ref_service" db:"ref_service"`     // order-core / payment-channel
	GrossAmountMinor int64    `json:"gross_amount_minor" db:"gross_amount_minor"`
	Currency        string    `json:"currency" db:"currency"`
	FeeMinor        int64     `json:"fee_minor" db:"fee_minor"`         // 实际收的 fee
	FeeMinorBase    int64     `json:"fee_minor_base" db:"fee_minor_base"` // 折算到 base currency 后
	FXRate          float64   `json:"fx_rate,omitempty" db:"fx_rate"`   // 转换汇率（若 currency != base）
	RuleID          int64     `json:"rule_id" db:"rule_id"`             // 命中哪条 rule
	RuleName        string    `json:"rule_name" db:"rule_name"`         // snapshot 防 rule 改名
	Product         string    `json:"product" db:"product"`
	ChannelAdapter  string    `json:"channel_adapter" db:"channel_adapter"`
	Region          string    `json:"region" db:"region"`
	Status          EventStatus `json:"status" db:"status"`             // pending / settled / void
	StatementID     int64     `json:"statement_id,omitempty" db:"statement_id"` // 归属哪期账单
	TraceID         string    `json:"trace_id,omitempty" db:"trace_id"`
	OccurredAt      time.Time `json:"occurred_at" db:"occurred_at"`     // 原始交易时间
	CreatedAt       time.Time `json:"created_at" db:"created_at"`
}

// EventType 费用事件分类。
type EventType string

const (
	EventCharge     EventType = "charge"     // 正常扣费
	EventRefund     EventType = "refund"     // 退款（可能 fee 退/不退）
	EventChargeback EventType = "chargeback" // 拒付（fee 一般不退 + 罚款）
	EventAdjustment EventType = "adjustment" // 手工调整 — 运营对账后补/扣
	EventFXSpread   EventType = "fx_spread"  // 跨币种 FX markup（独立 line item）
)

// EventStatus 状态机。
type EventStatus string

const (
	StatusPending EventStatus = "pending" // 已计算，但还没归入账单期
	StatusSettled EventStatus = "settled" // 已归入账单 + 商户能看
	StatusVoid    EventStatus = "void"    // 误算被撤销（不进账单）
)

// Statement 月度（或自定义周期）商户账单。
//
// 数据库存：billing_db_<shard>.statement
// PDF 渲染走 pdf/render.go，存 S3 / 本地路径。
type Statement struct {
	ID             int64     `json:"id" db:"id"`
	MerchantID     string    `json:"merchant_id" db:"merchant_id"`
	PeriodStart    time.Time `json:"period_start" db:"period_start"` // 含
	PeriodEnd      time.Time `json:"period_end" db:"period_end"`     // 不含 (exclusive)
	Currency       string    `json:"currency" db:"currency"`         // base currency for statement
	TotalGrossMinor int64    `json:"total_gross_minor" db:"total_gross_minor"`
	TotalFeeMinor  int64     `json:"total_fee_minor" db:"total_fee_minor"`
	TotalRefundMinor int64   `json:"total_refund_minor" db:"total_refund_minor"`
	TotalChargebackMinor int64 `json:"total_chargeback_minor" db:"total_chargeback_minor"`
	NetPayoutMinor int64     `json:"net_payout_minor" db:"net_payout_minor"`   // = total_gross - total_fee - refund - chargeback
	EventCount     int       `json:"event_count" db:"event_count"`
	Status         StmtStatus `json:"status" db:"status"`            // draft / final / paid
	PDFURL         string    `json:"pdf_url,omitempty" db:"pdf_url"`
	IssuedAt       time.Time `json:"issued_at" db:"issued_at"`     // 账单生成时间
	PaidAt         *time.Time `json:"paid_at,omitempty" db:"paid_at"` // 商户提现完成时间
	PayoutID       int64     `json:"payout_id,omitempty" db:"payout_id"`
}

// StmtStatus 账单状态。
type StmtStatus string

const (
	StmtDraft StmtStatus = "draft" // 周期未结束 / 还在累计
	StmtFinal StmtStatus = "final" // 已 freeze，商户能看
	StmtPaid  StmtStatus = "paid"  // 净额已支付给商户银行账户
	StmtVoid  StmtStatus = "void"  // 作废（仅运营特殊场景）
)

// Adjustment 手工调整（财务/运营给商户补/扣钱）。
type Adjustment struct {
	ID          int64     `json:"id" db:"id"`
	MerchantID  string    `json:"merchant_id" db:"merchant_id"`
	AmountMinor int64     `json:"amount_minor" db:"amount_minor"` // 正=补给商户（减 fee）；负=扣商户
	Currency    string    `json:"currency" db:"currency"`
	Reason      string    `json:"reason" db:"reason"`
	CreatedBy   string    `json:"created_by" db:"created_by"`      // ops email
	ApprovedBy  string    `json:"approved_by,omitempty" db:"approved_by"`
	StatementID int64     `json:"statement_id,omitempty" db:"statement_id"`
	CreatedAt   time.Time `json:"created_at" db:"created_at"`
}

// TransactionInput 入参 — 调用方（order-core / payment-channel 事件触发）给的数据。
// fee engine 不直接看 raw event，统一过这个结构。
type TransactionInput struct {
	MerchantID     string    `json:"merchant_id"`
	RefID          string    `json:"ref_id"`
	RefService     string    `json:"ref_service"`
	EventType      EventType `json:"event_type"`
	AmountMinor    int64     `json:"amount_minor"`
	Currency       string    `json:"currency"`
	Product        string    `json:"product"`
	ChannelAdapter string    `json:"channel_adapter"`
	Region         string    `json:"region"`
	CardBIN        string    `json:"card_bin,omitempty"`
	MerchantTier   string    `json:"merchant_tier,omitempty"`
	TraceID        string    `json:"trace_id,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// Package domain — split-payment 领域类型。
package domain

import "time"

// Rule 一条拆分规则 (商户后台创建, 多个交易共用)。
type Rule struct {
	ID             int64      `db:"id" json:"id"`
	MerchantID     string     `db:"merchant_id" json:"merchant_id"`
	Name           string     `db:"name" json:"name"`
	Items          []RuleItem `db:"-" json:"items"` // 序列化存 rule_items 表或 JSON
	HoldPeriodDays int        `db:"hold_period_days" json:"hold_period_days"`
	Status         string     `db:"status" json:"status"` // active / archived
	Currency       string     `db:"currency" json:"currency"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// RuleItem 拆分规则一项。
type RuleItem struct {
	Position       int    `json:"position"`        // 执行顺序 (从小到大; 余额按顺序扣)
	Beneficiary    string `json:"beneficiary"`     // 收款方 account_id / "{placeholder}"
	FromAttribute  string `json:"from_attribute"`  // 若 beneficiary 是 "{xxx}", 从 Attributes 取
	Type           string `json:"type"`            // percent / fixed_minor / remainder
	Value          int64  `json:"value"`           // type=percent: basis points (10000=100%)
	                                                //  type=fixed_minor: cents
	                                                //  type=remainder: ignore
	MinAmountMinor int64  `json:"min_amount_minor"` // 小于此不分给该方 (avoid dust)
	MaxAmountMinor int64  `json:"max_amount_minor"` // cap; 0 = 不 cap
	Optional       bool   `json:"optional"`        // attribute 不存在不拆 (不报错)
}

// Plan 一次交易的具体拆分方案 (实例化的 Rule)。
type Plan struct {
	ID         int64     `db:"id" json:"id"`
	RuleID     int64     `db:"rule_id" json:"rule_id"`
	ChargeID   string    `db:"charge_id" json:"charge_id"`
	MerchantID string    `db:"merchant_id" json:"merchant_id"`
	AmountMinor int64    `db:"amount_minor" json:"amount_minor"`
	Currency   string    `db:"currency" json:"currency"`
	Items      []PlanItem `db:"-" json:"items"`
	Status     string    `db:"status" json:"status"` // created/calculated/executing/completed/failed/reversed
	HoldUntil  *time.Time `db:"hold_until" json:"hold_until,omitempty"`
	ErrorMsg   string    `db:"error_msg" json:"error_msg,omitempty"`
	TraceID    string    `db:"trace_id" json:"trace_id"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time `db:"updated_at" json:"updated_at"`
}

// PlanItem 计算后的实际金额 (单位 minor)。
type PlanItem struct {
	Position    int    `json:"position"`
	Beneficiary string `json:"beneficiary"`
	AmountMinor int64  `json:"amount_minor"`
	Status      string `json:"status"` // pending / ledger_posted / failed
	LedgerEntryID int64 `json:"ledger_entry_id,omitempty"`
	Reason      string `json:"reason,omitempty"` // 出错原因
}

// LegacyReversal SP-1 时代的反向拆分 (基于 Plan + Items 拼分录).
//
// 跟 transfer.go 里 SP-3 Stripe-style Reversal 并存:
//   - LegacyReversal: 老 Plan-based 退款链 (execute.go Reverse 用)
//   - Reversal       : Stripe-style 单 Transfer 反向 (saga / refund handler / stripe API 用)
//
// 历史更名: 原本两个都叫 `Reversal`, 编译撞名. SP-AC-7 把老的改名兼容并存,
// 等老路径清完再删 LegacyReversal.
type LegacyReversal struct {
	ID             int64          `db:"id" json:"id"`
	OriginalPlanID int64          `db:"original_plan_id" json:"original_plan_id"`
	RefundID       string         `db:"refund_id" json:"refund_id"`
	AmountMinor    int64          `db:"amount_minor" json:"amount_minor"` // 部分退款支持
	Items          []ReversalItem `db:"-" json:"items"`
	Status         string         `db:"status" json:"status"`
	CreatedAt      time.Time      `db:"created_at" json:"created_at"`
}

// ReversalItem 反向扣款一项。
type ReversalItem struct {
	Beneficiary    string `json:"beneficiary"`
	AmountMinor    int64  `json:"amount_minor"`
	Shortfall      int64  `json:"shortfall"`       // 卖家余额不够, 平台先垫付
	PlatformCovered bool  `json:"platform_covered"`
}

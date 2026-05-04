package model

import "time"

// SettlementEvent Kafka 结算事件
// Redis 热路径成功后投递；Consumer 异步落库到 MySQL。
type SettlementEvent struct {
	VoucherNo    string              `json:"voucher_no"`
	BusinessNo   string              `json:"business_no"`
	BusinessType BusinessType        `json:"business_type"`
	Currency     string              `json:"currency"`
	Description  string              `json:"description"`
	Entries      []SettlementEntry   `json:"entries"`
	TransactAt   time.Time           `json:"transact_at"`
}

// SettlementEntry 单条分录结算记录
type SettlementEntry struct {
	TransactionID string `json:"transaction_id"`
	AccountNo     string `json:"account_no"`
	DebitAmount   string `json:"debit_amount"`
	CreditAmount  string `json:"credit_amount"`
	BalanceDelta  string `json:"balance_delta"`    // 净增量（正=增加，负=减少）
	BalanceBefore string `json:"balance_before"`   // Redis 变更前余额（供流水记录用）
	BalanceAfter  string `json:"balance_after"`    // Redis 变更后余额
}

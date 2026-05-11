// Package domain — Wallet 领域类型。
//
// 设计:
//   - Wallet 不持账, 余额 = accounting account "cust_wallet/{owner}/{currency}" 当前 balance
//   - 这里只存 wallet 元数据 (owner_type, status, kyc_state) + 业务交易记录 (审计/对账冗余)

package domain

import "time"

// Wallet 一个用户/商户的钱包元数据。
type Wallet struct {
	ID         int64  `db:"id" json:"id"`
	OwnerType  string `db:"owner_type" json:"owner_type"` // customer / merchant
	OwnerID    string `db:"owner_id" json:"owner_id"`
	Status     string `db:"status" json:"status"`         // active / frozen / closed
	KYCState   string `db:"kyc_state" json:"kyc_state"`   // none / pending / passed / failed
	DailyLimitMinor int64 `db:"daily_limit_minor" json:"daily_limit_minor"` // 0=不限
	Currencies map[string]bool `db:"-" json:"currencies"` // 已激活币种 (cache, accounting 才是真)
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	FrozenAt   *time.Time `db:"frozen_at" json:"frozen_at,omitempty"`
	FrozenReason string `db:"frozen_reason" json:"frozen_reason,omitempty"`
}

// Balance 单币种余额快照。
type Balance struct {
	Currency      string `json:"currency"`
	AvailableMinor int64 `json:"available_minor"` // 可用 (扣掉 hold)
	PendingMinor   int64 `json:"pending_minor"`   // hold / 待结算
	TotalMinor     int64 `json:"total_minor"`     // available + pending
}

// Transaction 钱包侧的业务交易记录 (跟 accounting voucher 一一对应, 冗余存便于查询)。
type Transaction struct {
	ID            int64     `db:"id" json:"id"`
	TxnID         string    `db:"txn_id" json:"txn_id"` // txn_xxx
	OwnerType     string    `db:"owner_type" json:"owner_type"`
	OwnerID       string    `db:"owner_id" json:"owner_id"`
	Type          string    `db:"type" json:"type"`              // topup / pay / withdraw / transfer / fx_convert / refund / freeze
	Currency      string    `db:"currency" json:"currency"`
	AmountMinor   int64     `db:"amount_minor" json:"amount_minor"`
	Direction     string    `db:"direction" json:"direction"`    // in / out
	Counterparty  string    `db:"counterparty" json:"counterparty,omitempty"` // 对方账户描述
	RefID         string    `db:"ref_id" json:"ref_id,omitempty"` // 关联 charge_id / payout_id / 等
	VoucherNo     string    `db:"voucher_no" json:"voucher_no,omitempty"` // accounting 凭证号
	Status        string    `db:"status" json:"status"`          // pending / completed / failed / reversed
	FailureReason string    `db:"failure_reason" json:"failure_reason,omitempty"`
	Metadata      map[string]string `db:"-" json:"metadata,omitempty"`
	CreatedAt     time.Time `db:"created_at" json:"created_at"`
}

// AccountID 根据 wallet 字段渲染出 accounting 账户名。
// 格式: cust_wallet/{owner_id}/{currency}    e.g. cust_wallet/u_001/USD
//       merc_wallet/{owner_id}/{currency}
func AccountID(ownerType, ownerID, currency string) string {
	prefix := "cust_wallet"
	if ownerType == "merchant" {
		prefix = "merc_wallet"
	}
	return prefix + "/" + ownerID + "/" + currency
}

// Constants
const (
	StatusActive  = "active"
	StatusFrozen  = "frozen"
	StatusClosed  = "closed"

	TypeTopup     = "topup"
	TypePay       = "pay"
	TypeWithdraw  = "withdraw"
	TypeTransfer  = "transfer"
	TypeFXConvert = "fx_convert"
	TypeRefund    = "refund"
	TypeFreeze    = "freeze"

	TxStatusPending   = "pending"
	TxStatusCompleted = "completed"
	TxStatusFailed    = "failed"
	TxStatusReversed  = "reversed"
)

// transfer.go — SP-2: Transfer / ApplicationFee / Payout / Reversal 四个一等资金原语.
//
// 仿 Stripe Connect 把"资金移动"显式建模,而非 Graph.Movement 隐式.
// 每个对象独立存表 + 独立状态机 + 独立 reversal/refund 链.
//
// 关系图:
//
//	Charge (order-core)
//	  ├── TransferGroup tg_xxx
//	  │     ├── Transfer tr_a → seller account
//	  │     ├── Transfer tr_b → referrer account
//	  │     └── Transfer tr_c → logistics account
//	  └── ApplicationFee fee_x → platform
//
//	Refund (refund-engine) → Reversal × N (按 transfer_group 反向冲销)
//
//	Account → Payout (T+N 银行通道)
package domain

import "time"

// ─── Transfer ─────────────────────────────────────────────────────────

// TransferStatus — 状态机标记.
const (
	TransferStatusCreated           = "created"
	TransferStatusPosted            = "posted"
	TransferStatusFailed            = "failed"
	TransferStatusReversed          = "reversed"
	TransferStatusPartiallyReversed = "partially_reversed"
)

// Transfer 一笔资金转移 (平台 → connected account, 或 connected → connected).
type Transfer struct {
	ID            string `db:"id" json:"id"` // tr_xxx
	TransferGroup string `db:"transfer_group" json:"transfer_group"`

	SourceAccount      string `db:"source_account" json:"source_account"`           // 默认 "platform" 平台主账户
	DestinationAccount string `db:"destination_account" json:"destination_account"` // acct_xxx
	AmountMinor        int64  `db:"amount_minor" json:"amount_minor"`
	Currency           string `db:"currency" json:"currency"`

	Description       string `db:"description" json:"description,omitempty"`
	SourceTransaction string `db:"source_transaction" json:"source_transaction,omitempty"` // 关联的 charge_id / pi_id
	ApplicationFee    string `db:"application_fee" json:"application_fee,omitempty"`       // 关联 ApplicationFee.ID

	Status         string `db:"status" json:"status"`
	ReversedAmount int64  `db:"reversed_amount" json:"reversed_amount"` // 累计被 reverse, ≤ amount_minor

	GraphRunID     int64  `db:"graph_run_id" json:"graph_run_id"`
	IdempotencyKey string `db:"idempotency_key" json:"idempotency_key"`

	Metadata map[string]string `db:"-" json:"metadata,omitempty"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	PostedAt  time.Time `db:"posted_at" json:"posted_at,omitempty"`
}

// RemainingReversible 还能 reverse 多少 (amount - 已 reversed).
func (t *Transfer) RemainingReversible() int64 {
	if t == nil {
		return 0
	}
	return t.AmountMinor - t.ReversedAmount
}

// ─── ApplicationFee ───────────────────────────────────────────────────

// ApplicationFeeStatus — 状态机.
const (
	AppFeeStatusPending            = "pending"
	AppFeeStatusCollected          = "collected"
	AppFeeStatusRefunded           = "refunded"
	AppFeeStatusPartiallyRefunded  = "partially_refunded"
)

// ApplicationFee 平台从 Charge 抽的手续费.
//
// Stripe 不把它当普通 Transfer, 因为:
//   - 财务侧分别核算: 营收 vs 代收代付
//   - 退款时按 graph.reversal.refund_application_fee 决定是否一起退
type ApplicationFee struct {
	ID             string `db:"id" json:"id"` // fee_xxx
	Charge         string `db:"charge" json:"charge"`           // 关联 charge_id
	Account        string `db:"account" json:"account,omitempty"` // 抽哪个商户的 fee (空 = 平台自营无抽成)
	AmountMinor    int64  `db:"amount_minor" json:"amount_minor"`
	Currency       string `db:"currency" json:"currency"`
	Status         string `db:"status" json:"status"`
	RefundedAmount int64  `db:"refunded_amount" json:"refunded_amount"`

	GraphRunID     int64             `db:"graph_run_id" json:"graph_run_id"`
	IdempotencyKey string            `db:"idempotency_key" json:"idempotency_key"`
	Metadata       map[string]string `db:"-" json:"metadata,omitempty"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// RemainingRefundable 还能 refund 多少.
func (f *ApplicationFee) RemainingRefundable() int64 {
	if f == nil {
		return 0
	}
	return f.AmountMinor - f.RefundedAmount
}

// ─── Payout ───────────────────────────────────────────────────────────

// PayoutStatus — 状态机.
const (
	PayoutStatusPending   = "pending"
	PayoutStatusInTransit = "in_transit"
	PayoutStatusPaid      = "paid"
	PayoutStatusFailed    = "failed"
	PayoutStatusCanceled  = "canceled"
)

// PayoutMethod — 提现速度档位.
const (
	PayoutMethodStandard = "standard" // T+2, 免费
	PayoutMethodInstant  = "instant"  // 秒到, 加 1% fee
)

// Payout connected account → 银行 / 钱包 的提现.
type Payout struct {
	ID          string `db:"id" json:"id"` // po_xxx
	Account     string `db:"account" json:"account"`
	AmountMinor int64  `db:"amount_minor" json:"amount_minor"`
	Currency    string `db:"currency" json:"currency"`

	Destination         PayoutDestination `db:"-" json:"destination"`
	Method              string            `db:"method" json:"method"`
	Status              string            `db:"status" json:"status"`
	ArrivalDate         time.Time         `db:"arrival_date" json:"arrival_date,omitempty"`
	FailureCode         string            `db:"failure_code" json:"failure_code,omitempty"`
	FailureMessage      string            `db:"failure_message" json:"failure_message,omitempty"`
	StatementDescriptor string            `db:"statement_descriptor" json:"statement_descriptor,omitempty"`

	// GraphRunID 若 payout 由 graph 自动触发则填; manual / cron 触发为 0.
	GraphRunID     int64             `db:"graph_run_id" json:"graph_run_id"`
	IdempotencyKey string            `db:"idempotency_key" json:"idempotency_key"`
	Metadata       map[string]string `db:"-" json:"metadata,omitempty"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// ─── Reversal ─────────────────────────────────────────────────────────

// ReversalStatus.
const (
	ReversalStatusPending   = "pending"
	ReversalStatusSucceeded = "succeeded"
	ReversalStatusFailed    = "failed"
)

// ReversalReason — Stripe-aligned.
const (
	ReversalReasonDuplicate            = "duplicate"
	ReversalReasonFraudulent           = "fraudulent"
	ReversalReasonRequestedByCustomer  = "requested_by_customer"
	ReversalReasonExpiredUncaptured    = "expired_uncaptured_charge"
)

// Reversal Transfer 的反向 (退款时把已分账的钱收回).
//
// 支持部分 reverse: 一个 Transfer 可有多个 Reversal,
// 累计 reversed_amount ≤ transfer.amount_minor.
//
// 幂等: 同 idempotency_key (一般 = refund_id + transfer_id) 不会重复执行.
type Reversal struct {
	ID             string `db:"id" json:"id"` // tr_rev_xxx
	Transfer       string `db:"transfer" json:"transfer"`
	AmountMinor    int64  `db:"amount_minor" json:"amount_minor"`
	Currency       string `db:"currency" json:"currency"`
	Reason         string `db:"reason" json:"reason"`
	Status         string `db:"status" json:"status"`
	FailureMessage string `db:"failure_message" json:"failure_message,omitempty"`

	IdempotencyKey string            `db:"idempotency_key" json:"idempotency_key"`
	GraphRunID     int64             `db:"graph_run_id" json:"graph_run_id"`
	Metadata       map[string]string `db:"-" json:"metadata,omitempty"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

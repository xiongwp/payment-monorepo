// payout.go — 商户提现 + 银行账户 + 准备金 domain.
//
// 4 大新增实体（在 model.go 既有 SettlementRun/Record 之外）:
//
//   MerchantBankAccount  — 商户绑定的银行账户（用于提现）
//   Payout               — 一笔提现请求（merchant → bank account）
//   ReserveAccount       — 准备金账户（防 chargeback / refund 拒付）
//   PayoutStatement      — 商户级结算单（多 statement 聚合 → 一笔 payout）
//
// 工作流：
//
//   billing-system 出 Statement (final)
//      ↓ (cron 02:00 每日 / 月初)
//   clearing-settlement 把同一 merchant 的 final statements 聚合成 PayoutStatement
//      ↓ (扣准备金 + 扣 reserve hold + 扣 pending dispute)
//   生成 Payout (status=pending_approval)
//      ↓ (ops 审批 — 大额 4-eyes)
//   Payout (status=approved)
//      ↓ (cron 调 bank API)
//   Payout (status=sent → settled / failed)

package domain

import "time"

// MerchantBankAccount 商户银行账户。
//
// 商户后台 KYB 后绑定；ops 审核通过后才能用于 payout。
// PII 字段需要 KMS 加密（account_number_enc 走 kms-manage）。
type MerchantBankAccount struct {
	ID                int64     `db:"id" json:"id"`
	MerchantID        string    `db:"merchant_id" json:"merchant_id"`
	HolderName        string    `db:"holder_name" json:"holder_name"`
	BankName          string    `db:"bank_name" json:"bank_name"`
	BankCountry       string    `db:"bank_country" json:"bank_country"`   // ISO-3166 alpha-2
	BankCode          string    `db:"bank_code" json:"bank_code"`         // SWIFT / BIC / IFSC / etc
	AccountNumberEnc  string    `db:"account_number_enc" json:"-"`        // KMS 加密
	AccountNumberLast4 string   `db:"account_number_last4" json:"account_number_last4"` // 给 UI 显示
	Currency          string    `db:"currency" json:"currency"`
	AccountType       string    `db:"account_type" json:"account_type"`   // checking / savings / business
	Status            BankAcctStatus `db:"status" json:"status"`          // pending_review / verified / suspended
	VerifiedAt        *time.Time `db:"verified_at" json:"verified_at,omitempty"`
	VerifiedBy        string    `db:"verified_by" json:"verified_by,omitempty"`
	RejectReason      string    `db:"reject_reason" json:"reject_reason,omitempty"`
	Default           bool      `db:"is_default" json:"is_default"`       // 商户多账户时默认走这个
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time `db:"updated_at" json:"updated_at"`
}

// BankAcctStatus 银行账户审核状态。
type BankAcctStatus string

const (
	BankAcctPending   BankAcctStatus = "pending_review"
	BankAcctVerified  BankAcctStatus = "verified"
	BankAcctSuspended BankAcctStatus = "suspended"
	BankAcctRejected  BankAcctStatus = "rejected"
)

// Payout 一笔商户提现。
//
// 创建路径有两条:
//   1. 自动 — clearing-settlement cron 跑 PayoutStatement → 自动生成 Payout
//   2. 手动 — 商户后台 / ops 触发，金额从可用余额扣
type Payout struct {
	ID                int64        `db:"id" json:"id"`
	MerchantID        string       `db:"merchant_id" json:"merchant_id"`
	BankAccountID     int64        `db:"bank_account_id" json:"bank_account_id"`
	PayoutStatementID int64        `db:"payout_statement_id" json:"payout_statement_id,omitempty"` // 关联结算单
	AmountMinor       int64        `db:"amount_minor" json:"amount_minor"`         // 净到账金额
	GrossMinor        int64        `db:"gross_minor" json:"gross_minor"`           // 扣手续费前
	BankFeeMinor      int64        `db:"bank_fee_minor" json:"bank_fee_minor"`     // 银行端转账费用
	Currency          string       `db:"currency" json:"currency"`
	Status            PayoutStatus `db:"status" json:"status"`
	BankRef           string       `db:"bank_ref" json:"bank_ref,omitempty"`       // 银行回执号
	FailureCode       string       `db:"failure_code" json:"failure_code,omitempty"`
	FailureMessage    string       `db:"failure_message" json:"failure_message,omitempty"`
	RequestedBy       string       `db:"requested_by" json:"requested_by"`         // 'merchant'|'ops'|'cron'
	ApprovedBy        string       `db:"approved_by" json:"approved_by,omitempty"` // ops 复核（大额）
	RequestedAt       time.Time    `db:"requested_at" json:"requested_at"`
	ApprovedAt        *time.Time   `db:"approved_at" json:"approved_at,omitempty"`
	SentAt            *time.Time   `db:"sent_at" json:"sent_at,omitempty"`         // 提交银行时间
	SettledAt         *time.Time   `db:"settled_at" json:"settled_at,omitempty"`   // 银行确认到账
	IdempotencyKey    string       `db:"idempotency_key" json:"idempotency_key"`   // 防重复提交
	TraceID           string       `db:"trace_id" json:"trace_id,omitempty"`
	CreatedAt         time.Time    `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time    `db:"updated_at" json:"updated_at"`
}

// PayoutStatus 提现状态机:
//
//	pending_approval (>$10k 等大额触发) → approved → sent → settled
//	                                                      ↘ failed (银行退回)
//	pending_approval ──────────────────────────────────────→ rejected (ops 拒)
//	approved ──→ canceled (商户改主意)
type PayoutStatus string

const (
	PayoutPendingApproval PayoutStatus = "pending_approval"
	PayoutApproved        PayoutStatus = "approved"
	PayoutSent            PayoutStatus = "sent"
	PayoutSettled         PayoutStatus = "settled"
	PayoutFailed          PayoutStatus = "failed"
	PayoutRejected        PayoutStatus = "rejected"
	PayoutCanceled        PayoutStatus = "canceled"
)

// ReserveAccount 商户准备金账户（防 chargeback 资金风险）。
//
// 准备金策略（business 定）:
//   - 新商户首 90 天: 留 5% gross volume
//   - 高 chargeback 率 (>1%): 留 10%
//   - 跨境 / 高风险品类: 单独配额
//
// Reserve hold 释放节奏: 每周/每月 release 一笔满 90 天的 hold。
type ReserveAccount struct {
	ID            int64     `db:"id" json:"id"`
	MerchantID    string    `db:"merchant_id" json:"merchant_id"`
	Currency      string    `db:"currency" json:"currency"`
	BalanceMinor  int64     `db:"balance_minor" json:"balance_minor"`         // 总余额（已 hold + 已 release pending）
	HeldMinor     int64     `db:"held_minor" json:"held_minor"`               // 仍在锁定期
	ReleasableMinor int64   `db:"releasable_minor" json:"releasable_minor"`   // 已到期可释放
	Policy        string    `db:"policy" json:"policy"`                       // "new_merchant_5pct" / "high_cb_10pct" / "custom"
	ReleaseDays   int       `db:"release_days" json:"release_days"`           // 锁定天数（默认 90）
	UpdatedAt     time.Time `db:"updated_at" json:"updated_at"`
}

// ReserveHold 一笔具体的 reserve hold（关联 payout statement → 知道为啥扣）。
type ReserveHold struct {
	ID             int64     `db:"id" json:"id"`
	MerchantID     string    `db:"merchant_id" json:"merchant_id"`
	StatementID    int64     `db:"statement_id" json:"statement_id"`           // 触发本次 hold 的 statement
	AmountMinor    int64     `db:"amount_minor" json:"amount_minor"`
	Currency       string    `db:"currency" json:"currency"`
	HoldUntil      time.Time `db:"hold_until" json:"hold_until"`               // 解禁日
	Released       bool      `db:"released" json:"released"`
	ReleasedAt     *time.Time `db:"released_at" json:"released_at,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

// PayoutStatement 商户级结算单 —— 把同 merchant 多个 billing.Statement 聚成
// 一次"该给商户多少钱"的视图，扣 reserve + 扣 dispute pending + 扣 adjustments
// 后生成 Payout。
type PayoutStatement struct {
	ID                 int64     `db:"id" json:"id"`
	MerchantID         string    `db:"merchant_id" json:"merchant_id"`
	Currency           string    `db:"currency" json:"currency"`
	PeriodStart        time.Time `db:"period_start" json:"period_start"`
	PeriodEnd          time.Time `db:"period_end" json:"period_end"`
	BillingStatementIDs string   `db:"billing_statement_ids" json:"billing_statement_ids"` // CSV
	GrossPayoutMinor   int64     `db:"gross_payout_minor" json:"gross_payout_minor"`       // billing net_payout 总和
	ReserveHoldMinor   int64     `db:"reserve_hold_minor" json:"reserve_hold_minor"`       // 本期新扣的 reserve
	ReserveReleasedMinor int64   `db:"reserve_released_minor" json:"reserve_released_minor"` // 本期到期释放
	DisputePendingMinor int64    `db:"dispute_pending_minor" json:"dispute_pending_minor"` // 拒付未决保留
	AdjustmentMinor    int64     `db:"adjustment_minor" json:"adjustment_minor"`           // 手工调整
	NetPayoutMinor     int64     `db:"net_payout_minor" json:"net_payout_minor"`           // 实际打钱金额
	PayoutID           int64     `db:"payout_id" json:"payout_id,omitempty"`               // 关联生成的 Payout
	Status             string    `db:"status" json:"status"`                               // draft / final / paid
	IssuedAt           time.Time `db:"issued_at" json:"issued_at"`
	CreatedAt          time.Time `db:"created_at" json:"created_at"`
}

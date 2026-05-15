// account.go — SP-2: ConnectedAccount, 仿 Stripe Connect Account.
//
// 每个收款方 / 提现方 (商户 / 卖家 / 推广员 / 物流方 / 清结算账户) 是独立的
// 一等 ConnectedAccount 对象, 有自己的 KYC / capabilities / payout 配置.
//
// 跟 user-merchant-core.merchant 的关系:
//   - 业务侧的 merchant 是"商户实体", 一对一 (或一对多, 多店铺) 对应 ConnectedAccount.
//   - 我们 split-payment 不存 KYC 详情, 只持 ConnectedAccount.ID + capabilities + payout_destination.
//   - 详细 KYC 字段 (营业执照 / 法人身份证) 留在 user-merchant-core, 通过 Metadata 反查.
//
// 状态机:
//
//	pending  → enabled       (KYC 通过)
//	enabled  → restricted    (风控触发 / 文档过期, 不能 Transfer/Payout)
//	restricted → enabled     (整改)
//	*        → disabled      (注销)
//	*        → rejected      (拒绝入驻)
//
// 只 enabled 才能做 Transfer / Payout. restricted 仍可作为 Transfer 目标 (能收钱),
// 但禁出 (不能 Payout).
package domain

import "time"

// AccountType — Stripe 三档 (KYC 严格度递增).
const (
	AccountTypeStandard = "standard" // 商户自己注册 Stripe 账户, 平台只关联
	AccountTypeExpress  = "express"  // 平台代办 KYC, 简化流程
	AccountTypeCustom   = "custom"   // 平台完全代管, 商户无独立 Stripe 账户
)

// AccountStatus — 状态机标记.
const (
	AccountStatusPending    = "pending"
	AccountStatusEnabled    = "enabled"
	AccountStatusRestricted = "restricted"
	AccountStatusDisabled   = "disabled"
	AccountStatusRejected   = "rejected"
)

// Capability — 资金能力开关. value: "active" / "pending" / "inactive".
const (
	CapabilityTransfers    = "transfers"
	CapabilityCardPayments = "card_payments"
	CapabilityPayouts      = "payouts"
)

// PayoutScheduleInterval — 自动 payout 频率.
const (
	PayoutManual = "manual" // 商户主动发起
	PayoutDaily  = "daily"
	PayoutWeekly = "weekly"
	PayoutMonthly = "monthly"
)

// ConnectedAccount 一个收款 / 提现方.
type ConnectedAccount struct {
	ID              string `db:"id" json:"id"`         // acct_xxx
	Type            string `db:"type" json:"type"`     // standard / express / custom
	Country         string `db:"country" json:"country"` // ISO-2 (US / CN / DE ...)
	DefaultCurrency string `db:"default_currency" json:"default_currency"`

	BusinessProfile   BusinessProfile   `db:"-" json:"business_profile"`
	PayoutDestination PayoutDestination `db:"-" json:"payout_destination"`
	PayoutSchedule    PayoutSchedule    `db:"-" json:"payout_schedule"`

	// Capabilities: 能力开关. 见 CapabilityXxx 常量.
	// e.g. {"transfers": "active", "payouts": "pending"}
	Capabilities map[string]string `db:"-" json:"capabilities"`
	Status       string            `db:"status" json:"status"`

	// Metadata 用户自定义 KV, 也是反查 user-merchant-core.merchant 的桥.
	// 约定 metadata["merchant_id"] = 关联的 user-merchant-core merchant 主键.
	Metadata map[string]string `db:"-" json:"metadata"`

	CreatedAt time.Time `db:"created_at" json:"created_at"`
	UpdatedAt time.Time `db:"updated_at" json:"updated_at"`
}

// BusinessProfile 商户元信息. 不包含 KYC 文档详情 (那个在 user-merchant-core).
type BusinessProfile struct {
	Name              string `json:"name,omitempty"`
	URL               string `json:"url,omitempty"`
	SupportEmail      string `json:"support_email,omitempty"`
	SupportPhone      string `json:"support_phone,omitempty"`
	StatementDescriptor string `json:"statement_descriptor,omitempty"` // payout 时显示的商户名
	ProductDescription string `json:"product_description,omitempty"`
}

// PayoutDestination 提现目的地.
type PayoutDestination struct {
	Kind          string `json:"kind"` // "bank_account" / "wallet" / "card"
	BankCountry   string `json:"bank_country,omitempty"`
	Currency      string `json:"currency,omitempty"`
	Last4         string `json:"last4,omitempty"`         // 银行卡尾号
	AccountHolder string `json:"account_holder,omitempty"`
	// 真实账号走 tokenization-vault 服务, 这里只持 token 引用
	TokenRef string `json:"token_ref,omitempty"`
}

// PayoutSchedule 自动 payout 节奏.
type PayoutSchedule struct {
	Interval     string `json:"interval"`               // manual / daily / weekly / monthly
	WeeklyAnchor string `json:"weekly_anchor,omitempty"` // monday / tuesday / ... (weekly 用)
	MonthlyAnchor int   `json:"monthly_anchor,omitempty"` // 1-31 (monthly 用, 31 表示月底)
	DelayDays     int   `json:"delay_days,omitempty"`     // T+N, 风控隔离期
}

// CanTransfer 决定能否被作为 Transfer 目标 (能收).
//
// enabled / restricted 都可收 (restricted 只是不能出).
func (a *ConnectedAccount) CanTransfer() bool {
	if a == nil {
		return false
	}
	if a.Status != AccountStatusEnabled && a.Status != AccountStatusRestricted {
		return false
	}
	return a.Capabilities[CapabilityTransfers] == "active"
}

// CanPayout 决定能否发起 Payout (出钱).
//
// 仅 enabled 状态 + payouts capability active.
func (a *ConnectedAccount) CanPayout() bool {
	if a == nil {
		return false
	}
	if a.Status != AccountStatusEnabled {
		return false
	}
	return a.Capabilities[CapabilityPayouts] == "active"
}

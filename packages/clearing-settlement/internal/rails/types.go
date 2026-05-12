package rails

import "time"

// Network 资金通道类型
type Network string

const (
	NetworkACH        Network = "ach"        // 美国 NACHA
	NetworkSEPA       Network = "sepa"       // EU SEPA Credit Transfer (pain.001)
	NetworkWireSWIFT  Network = "swift_mt103" // 跨境 SWIFT
	NetworkFedwire    Network = "fedwire"    // 美国大额 (>$100k 一般走)
	NetworkUKFaster   Network = "uk_fps"     // UK Faster Payments
)

// BankAccount 银行账户 (我们和商户两侧都用同结构).
type BankAccount struct {
	HolderName   string `json:"holder_name"`
	AccountNumber string `json:"account_number"`     // NACHA / domestic
	IBAN         string `json:"iban,omitempty"`      // SEPA
	BIC          string `json:"bic,omitempty"`       // SWIFT 8 or 11 char
	RoutingNum   string `json:"routing_num,omitempty"` // NACHA ABA 9
	SortCode     string `json:"sort_code,omitempty"` // UK 6 digit
	Country      string `json:"country"`              // ISO-2
	BankName     string `json:"bank_name,omitempty"`
	BankAddress  string `json:"bank_address,omitempty"`
}

// Payout 一笔出款 — clearing-settlement 内 domain.Payout 的 subset.
type Payout struct {
	PayoutID    string    // 业务侧 ID
	Description string    // memo (≤ 80 字符 SWIFT, 255 NACHA)
	AmountCents int64     // 总额, 分
	Currency    string    // ISO 4217
	ValueDate   time.Time // 期望到账日 (NACHA effective date)
	SettleDate  time.Time // 我们这边出账日
	OurRef      string    // 我们的内部 reference, 跟 bank reco 关联
	EndToEndID  string    // ISO 20022 EndToEndIdentification — 跟 reconplatform 闭环
}

// Originator 我们 (平台公司) 出款方信息
type Originator struct {
	Name           string
	Account        BankAccount
	ID             string  // 公司在银行注册的 ID (NACHA Company ID 等)
	IdentifierType string  // EIN / DUNS
}

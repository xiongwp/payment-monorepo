// Package domain — 双层代币 (Two-Layer Token) 核心模型.
//
// 架构 (跟商户对外约定):
//
//   merchant 侧:
//     internal_token       — 商户 DB 主键, 不可还原 PAN, 永久有效
//
//   vault 侧 (这个服务):
//     internal_token       ↔  pan_hash  ↔  network_token (VTS/MDES)
//                                          │
//                                          └── cryptogram (单次扣款时生成)
//
//   推 transaction 到卡组:
//     PSP 拿 (network_token, cryptogram) 而不是 PAN
//
// 这一层把 "卡数据" 从商户 DB 抽出, 商户进 PCI scope reduction 边界 (SAQ-A → SAQ-D 降级).

package domain

import "time"

// InternalToken 商户面 token — 商户保存这个做 DB 主键.
// 永久稳定, 跟 PAN 一对一 (按 merchant 维度去重 — 同一个 PAN 不同商户得到不同 internal_token).
type InternalToken struct {
	Token       string      `json:"token"`         // 例: tk_live_abc123... (32 hex)
	MerchantID  string      `json:"merchant_id"`
	PANHash     string      `json:"pan_hash"`      // sha256(PAN)[:32] - 给 vault 内部 dedup
	PANLast4    string      `json:"pan_last4"`
	BIN         string      `json:"bin"`           // 前 6 位 (BIN range)
	Brand       CardBrand   `json:"brand"`
	ExpMonth    int         `json:"exp_month"`
	ExpYear     int         `json:"exp_year"`
	CardholderH string      `json:"cardholder_h"`  // sha256(name)[:16] - 用于 fuzzy match
	NetworkRef  *NetworkRef `json:"network_ref,omitempty"` // VTS/MDES 引用; nil = 还没 provisioning
	Status      TokenStatus `json:"status"`
	Created_at  time.Time   `json:"created_at"`
	Updated_at  time.Time   `json:"updated_at"`
}

// NetworkRef 跟卡组的网络代币的关联引用.
// 真实 PAN 由 issuer 在 VTS/MDES 服务端保留, vault 只持有 token reference.
type NetworkRef struct {
	Provider     TokenProvider `json:"provider"`           // vts | mdes | inhouse_fallback
	NetworkToken string        `json:"network_token"`      // 卡组返回的 PAR / DPAN
	TokenRefID   string        `json:"token_ref_id"`       // provider 内部 ref
	TokenExpiry  string        `json:"token_expiry"`       // MMYY
	ProvisionedAt time.Time    `json:"provisioned_at"`
}

// TokenStatus 生命周期
type TokenStatus string

const (
	TokenActive    TokenStatus = "active"
	TokenSuspended TokenStatus = "suspended" // 商户冻结 / 风控
	TokenDeleted   TokenStatus = "deleted"   // 软删, 不可再用
	TokenExpired   TokenStatus = "expired"   // 卡过期 (issuer notification)
)

// TokenProvider 代币 provider
type TokenProvider string

const (
	ProviderVTS      TokenProvider = "vts"       // Visa Token Service
	ProviderMDES     TokenProvider = "mdes"      // Mastercard Digital Enablement Service
	ProviderInhouse  TokenProvider = "inhouse"   // 自建 PCI vault fallback (无网络代币)
)

// CardBrand
type CardBrand string

const (
	BrandVisa       CardBrand = "visa"
	BrandMastercard CardBrand = "mastercard"
	BrandAmex       CardBrand = "amex"
	BrandDiscover   CardBrand = "discover"
	BrandJCB        CardBrand = "jcb"
	BrandUnionPay   CardBrand = "unionpay"
	BrandUnknown    CardBrand = "unknown"
)

// ── API 请求 / 响应 ──

// ExchangeRequest 把 PAN 换成 internal_token (一次性, PCI 边界内).
//
// 调用者: card-payment / payment-gateway (内部, PCI scope; 不让商户直接调).
// 商户调走 hosted iframe / SDK, PAN 永远不进商户 server.
type ExchangeRequest struct {
	PAN        string `json:"pan"`            // 调用方必须用 TLS + mTLS
	ExpMonth   int    `json:"exp_month"`
	ExpYear    int    `json:"exp_year"`
	Cardholder string `json:"cardholder"`
	CVV        string `json:"cvv,omitempty"`  // 不存, 只用于 first-tx (then discarded)
	MerchantID string `json:"merchant_id"`
}

type ExchangeResponse struct {
	Token     string    `json:"token"`       // internal_token
	PANLast4  string    `json:"pan_last4"`
	BIN       string    `json:"bin"`
	Brand     CardBrand `json:"brand"`
	ExpMonth  int       `json:"exp_month"`
	ExpYear   int       `json:"exp_year"`
	Status    TokenStatus `json:"status"`
	Provisioned bool    `json:"provisioned"` // 是否已 provision 到 VTS/MDES
}

// ChargeIntentRequest "我要扣这张 token N 块" → vault 返回 (network_token, cryptogram).
// PSP 拿这俩 + 业务字段提交卡组.
type ChargeIntentRequest struct {
	Token       string `json:"token"`        // internal_token
	MerchantID  string `json:"merchant_id"`
	Amount      int64  `json:"amount"`       // 分
	Currency    string `json:"currency"`     // ISO 4217
	Recurring   bool   `json:"recurring"`    // true = MIT (商户发起), false = CIT (用户在场)
	IntentID    string `json:"intent_id"`    // payment-core / order-core 的幂等 ID
}

type ChargeIntentResponse struct {
	NetworkToken string    `json:"network_token"`  // DPAN / PAR
	Cryptogram   string    `json:"cryptogram"`     // TAVV/UCAF base64
	ECI          string    `json:"eci"`            // Electronic Commerce Indicator (visa 05/06, mc 02)
	TokenExpiry  string    `json:"token_expiry"`   // MMYY
	Provider     TokenProvider `json:"provider"`
	IntentID     string    `json:"intent_id"`
}

// ProvisionRequest 给 vault 触发 VTS/MDES provisioning.
// 一般 vault 在第一次 exchange 后异步跑; admin API 用于手动重试 / 强制再 provision.
type ProvisionRequest struct {
	Token string `json:"token"` // internal_token
}

type ProvisionResponse struct {
	Token       string        `json:"token"`
	Provider    TokenProvider `json:"provider"`
	NetworkRef  NetworkRef    `json:"network_ref"`
}

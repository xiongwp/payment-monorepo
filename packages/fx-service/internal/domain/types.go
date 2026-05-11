// Package domain — FX 领域类型。
//
// 设计:
//   - 所有汇率以 USD 为 base, 减少 N×N 维护 (USD↔X)
//   - 汇率精度: 8 位小数 (用 int64 * 1e8 存, 防 float 累计误差)
//   - 每条 rate 有 source + fetched_at, 多源聚合时按 source 优先级 + 时间衰减选最佳

package domain

import "time"

// RateScale 汇率小数位 (8 位 = stripe / 行业惯例)。
const RateScale = 100_000_000

// Rate 一条汇率数据 (某时刻、某源、某对)。
type Rate struct {
	ID          int64     `db:"id" json:"id"`
	FromCurrency string   `db:"from_ccy" json:"from"`         // ISO 4217 (3 chars), e.g. "USD"
	ToCurrency  string    `db:"to_ccy" json:"to"`             // e.g. "JPY"
	MidRateE8   int64     `db:"mid_rate_e8" json:"mid_rate_e8"` // 1 from = X to, *1e8
	BidRateE8   int64     `db:"bid_rate_e8" json:"bid_rate_e8"` // 银行买入 (from→to 时实际能拿到)
	AskRateE8   int64     `db:"ask_rate_e8" json:"ask_rate_e8"` // 银行卖出
	Source      string    `db:"source" json:"source"`         // "ecb" / "oanda" / "stripe_fx" / "manual"
	FetchedAt   time.Time `db:"fetched_at" json:"fetched_at"`
	ExpiresAt   time.Time `db:"expires_at" json:"expires_at"` // 默认 5min
}

// Quote 报价 (商户/用户先报价, 在过期前 confirm 才执行 conversion)。
type Quote struct {
	ID           int64     `db:"id" json:"id"`
	QuoteID      string    `db:"quote_id" json:"quote_id"`
	FromCurrency string    `db:"from_ccy" json:"from"`
	ToCurrency   string    `db:"to_ccy" json:"to"`
	FromAmountMinor int64  `db:"from_amount_minor" json:"from_amount_minor"`
	ToAmountMinor   int64  `db:"to_amount_minor" json:"to_amount_minor"`
	RateE8       int64     `db:"rate_e8" json:"rate_e8"`         // 实际用的 rate (含 spread/fee)
	MidRateE8    int64     `db:"mid_rate_e8" json:"mid_rate_e8"` // 同时刻 mid (审计 spread)
	FeeMinor     int64     `db:"fee_minor" json:"fee_minor"`
	FeeCurrency  string    `db:"fee_ccy" json:"fee_currency"`
	SpreadBp     int       `db:"spread_bp" json:"spread_bp"`     // 基点 (10000 = 100%); 通常 50-100bp = 0.5%-1%
	MerchantID   string    `db:"merchant_id" json:"merchant_id,omitempty"`
	CustomerID   string    `db:"customer_id" json:"customer_id,omitempty"`
	Status       string    `db:"status" json:"status"`           // open / confirmed / expired / canceled
	ExpiresAt    time.Time `db:"expires_at" json:"expires_at"`   // 5min 锁价
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// Conversion 真实执行的兑换 (quote confirm 后的产物)。
type Conversion struct {
	ID           int64     `db:"id" json:"id"`
	ConversionID string    `db:"conversion_id" json:"conversion_id"`
	QuoteID      string    `db:"quote_id" json:"quote_id"`
	FromAccount  string    `db:"from_account" json:"from_account"`
	ToAccount    string    `db:"to_account" json:"to_account"`
	FromAmountMinor int64  `db:"from_amount_minor" json:"from_amount_minor"`
	ToAmountMinor   int64  `db:"to_amount_minor" json:"to_amount_minor"`
	FromCurrency string    `db:"from_ccy" json:"from"`
	ToCurrency   string    `db:"to_ccy" json:"to"`
	RateE8       int64     `db:"rate_e8" json:"rate_e8"`
	VoucherNo    string    `db:"voucher_no" json:"voucher_no"` // accounting 回填
	Status       string    `db:"status" json:"status"`         // pending/posted/failed/reversed
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// Source 汇率来源元数据 (admin 配置)。
type Source struct {
	Name     string `json:"name"`     // "ecb" / "oanda" / "stripe_fx" / ...
	Priority int    `json:"priority"` // 高优先级先选 (1 = 最高)
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	APIKey   string `json:"api_key,omitempty"`
}

// ─── 转换 helpers ──────────────────────────────────────────────────────

// ApplyRate from amount * rate → to amount (round half-up to nearest minor)。
func ApplyRate(fromAmount, rateE8 int64) int64 {
	// 用 int128 安全; Go 没原生, 这里用 big.Int 在 helper 里处理. 大额场景必备.
	// 简化: 假设 fromAmount * rateE8 < 2^63 (绝大多数支付场景安全).
	num := fromAmount * rateE8
	// 加 RateScale/2 实现 round-half-up
	return (num + RateScale/2) / RateScale
}

// ApplySpread mid * (1 + bp/10000) — 卖出价 (ask); buy 用 (1 - bp/10000).
func ApplySpread(midE8 int64, spreadBp int, isAsk bool) int64 {
	delta := midE8 * int64(spreadBp) / 10000
	if isAsk {
		return midE8 + delta
	}
	return midE8 - delta
}

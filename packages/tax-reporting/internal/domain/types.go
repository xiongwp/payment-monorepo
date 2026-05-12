// Package domain — 税务申报核心类型.
//
// 三大流程:
//   1. PayoutEvent ingest — 从 clearing-settlement / billing-system 持续收商户收款流水
//   2. 年度 aggregate — 按 (merchant_id, calendar_year, jurisdiction) 算累计收款 + 笔数
//   3. Form 生成 — 1099-K / W-8 / VAT OSS / GB MTD 出 PDF + e-file payload
//
// IRS 1099-K (2024+ 新规):
//   - 阈值: \$5000 (2024) → \$2500 (2025) → \$600 (2026+)
//   - 不再有 200 笔阈值 (老规则)
//   - 商户先填 W-9 (US) 或 W-8BEN/W-8BEN-E (非 US)
//
// EU VAT OSS:
//   - B2C 跨境数字服务月报 → 申报国 (one-stop-shop)
//   - 阈值: €10,000 跨境收入总和

package domain

import "time"

// PayoutEvent 单笔商户收款 — 由 clearing-settlement / billing-system 推送过来.
type PayoutEvent struct {
	EventID     string    `json:"event_id"`       // 幂等 key (源服务出)
	MerchantID  string    `json:"merchant_id"`
	OccurredAt  time.Time `json:"occurred_at"`    // 商户拿到钱的时间, 非授权时间
	GrossAmount int64     `json:"gross_amount"`   // 分; 申报口径是 gross (扣手续费前)
	NetAmount   int64     `json:"net_amount"`     // 实际打到商户的
	FeesAmount  int64     `json:"fees_amount"`    // gross - net (跟 billing-system 一致)
	Currency    string    `json:"currency"`       // ISO 4217
	Jurisdiction string   `json:"jurisdiction"`    // tax authority — US / EU / GB / DE / IN ...
	TxnCount    int       `json:"txn_count"`      // 这条 event 聚合的交易笔数 (一般 1, 月结可以批)
	Source      string    `json:"source"`         // billing / clearing / manual
	Channel     string    `json:"channel,omitempty"` // visa / mc / bank / paypal — 1099-K 要分渠道
}

// MerchantTaxProfile 商户税务档案. 申报前必须填齐.
type MerchantTaxProfile struct {
	MerchantID   string         `json:"merchant_id"`
	LegalName    string         `json:"legal_name"`
	TIN          string         `json:"tin,omitempty"`           // EIN / SSN (US) - hash; W-9 / W-8 完成后填
	TINType      TINType        `json:"tin_type"`                // ein / ssn / itin / foreign
	TIN_Hash     string         `json:"tin_hash,omitempty"`      // sha256(TIN)[:32]
	TaxClass     TaxClass       `json:"tax_class"`               // individual / sole_prop / c_corp / partnership / llc / foreign
	BusinessAddr Address        `json:"business_address"`
	W9_Submitted bool           `json:"w9_submitted"`
	W9_Date      time.Time      `json:"w9_date,omitempty"`
	W8_Submitted bool           `json:"w8_submitted"`
	W8_Date      time.Time      `json:"w8_date,omitempty"`
	W8_Type      string         `json:"w8_type,omitempty"`       // W-8BEN / W-8BEN-E / W-8ECI / W-8IMY
	Country      string         `json:"country"`                 // ISO-2; 决定走 1099 还是 W-8
	VAT_Number   string         `json:"vat_number,omitempty"`    // EU VAT (跨境 OSS 用)
	UpdatedAt    time.Time      `json:"updated_at"`
}

type TINType string

const (
	TINTypeEIN     TINType = "ein"      // 公司
	TINTypeSSN     TINType = "ssn"      // 个人
	TINTypeITIN    TINType = "itin"     // 非居民个人
	TINTypeForeign TINType = "foreign"  // 非 US (走 W-8)
)

type TaxClass string

const (
	TaxClassIndividual  TaxClass = "individual"
	TaxClassSoleProp    TaxClass = "sole_prop"
	TaxClassCCorp       TaxClass = "c_corp"
	TaxClassSCorp       TaxClass = "s_corp"
	TaxClassPartnership TaxClass = "partnership"
	TaxClassLLC         TaxClass = "llc"
	TaxClassForeign     TaxClass = "foreign"
)

type Address struct {
	Line1   string `json:"line1"`
	Line2   string `json:"line2,omitempty"`
	City    string `json:"city"`
	State   string `json:"state,omitempty"`
	Zip     string `json:"zip,omitempty"`
	Country string `json:"country"` // ISO-2
}

// Aggregate 年度按月分摊汇总.
// 一年 → 12 个月 grouped, 加每月 + 全年总.
type Aggregate struct {
	MerchantID   string         `json:"merchant_id"`
	Year         int            `json:"year"`
	Jurisdiction string         `json:"jurisdiction"`
	Currency     string         `json:"currency"`
	MonthlyGross [12]int64      `json:"monthly_gross"`  // index 0..11 = Jan..Dec
	MonthlyCount [12]int        `json:"monthly_count"`
	TotalGross   int64          `json:"total_gross"`
	TotalCount   int            `json:"total_count"`
	ByChannel    map[string]int64 `json:"by_channel"`   // visa: 12345, mc: 6789
	UpdatedAt    time.Time      `json:"updated_at"`
}

// FormType 申报类型
type FormType string

const (
	Form1099K    FormType = "1099-k"      // US payment settlement entity
	FormW9       FormType = "w-9"         // US taxpayer ID request
	FormW8BEN    FormType = "w-8ben"      // foreign individual
	FormW8BENE   FormType = "w-8ben-e"    // foreign entity
	FormVATOSS   FormType = "vat-oss"     // EU 跨境 OSS
	FormUKVAT    FormType = "uk-vat"      // GB MTD
)

// FormStatus 文件状态
type FormStatus string

const (
	FormDraft     FormStatus = "draft"      // 数据汇集中
	FormReady     FormStatus = "ready"      // 可下载 / 可申报
	FormFiled     FormStatus = "filed"      // 已申报给税务局 / 已提交商户
	FormCorrected FormStatus = "corrected"  // 已 amended
)

// TaxForm 单份申报文档
type TaxForm struct {
	FormID       string     `json:"form_id"`
	FormType     FormType   `json:"form_type"`
	MerchantID   string     `json:"merchant_id"`
	Year         int        `json:"year"`
	Jurisdiction string     `json:"jurisdiction"`
	GrossAmount  int64      `json:"gross_amount"`        // 分
	Currency     string     `json:"currency"`
	Status       FormStatus `json:"status"`
	GeneratedAt  time.Time  `json:"generated_at"`
	FiledAt      time.Time  `json:"filed_at,omitempty"`
	EFileID      string     `json:"efile_id,omitempty"`  // IRS FIRE / Avalara confirmation
	PDFRef       string     `json:"pdf_ref,omitempty"`   // S3 / GCS object key
	Payload      map[string]interface{} `json:"payload,omitempty"`
}

// FilingThreshold 触发申报的阈值规则.
// US 1099-K 2026 阈值 \$600 (gross / year), 2025 \$2500, 2024 \$5000.
type FilingThreshold struct {
	Jurisdiction string
	Year         int
	FormType     FormType
	MinGross     int64  // 分; 0 表示无下限 (W-8 这种)
	MinCount     int    // 笔数; 0 表示不算
}

// DefaultThresholds 当前各地区主要阈值.
func DefaultThresholds() []FilingThreshold {
	return []FilingThreshold{
		// US 1099-K — 2024 / 2025 阶梯下调, 2026 起 \$600
		{"US", 2024, Form1099K, 5000_00, 0},
		{"US", 2025, Form1099K, 2500_00, 0},
		{"US", 2026, Form1099K, 600_00, 0},
		// EU VAT OSS 跨境
		{"EU", 2025, FormVATOSS, 10_000_00, 0},
		{"EU", 2026, FormVATOSS, 10_000_00, 0},
		// UK MTD — 阈值 £90k turnover (90 000 00 pence)
		{"GB", 2026, FormUKVAT, 90_000_00, 0},
	}
}

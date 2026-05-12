// vat_oss.go — EU VAT One Stop Shop 月报.
//
// EU OSS: B2C 跨境数字 / 远程销售商户在单一注册国 (一般是经营国) 月报增值税.
// 月度报表 cycle: 每月 1 日截至前月 31 日的销售; 下月 20 日前申报 + 缴款.
// 阈值: €10,000 累计 (整 EU); 超阈值后必须 OSS, 没超可走本国销售税.

package forms

import (
	"fmt"
	"time"

	"reconcile-system/packages/tax-reporting/internal/domain"
)

// GenerateVATOSS 月度 OSS 申报 — 输入: 商户在 EU 各国的销售拆分.
type VATOSSInput struct {
	MerchantID   string
	Year         int
	Month        int                  // 1..12
	HomeCountry  string               // 商户注册国 ISO-2
	ByCountry    map[string]VATCountryItem // ISO-2 → 销售明细
}

type VATCountryItem struct {
	NetSales  int64   // 分; 不含税
	VATRate   float64 // e.g. 0.21 (DE), 0.20 (FR)
	VATAmount int64   // 分; round(NetSales * VATRate)
}

func GenerateVATOSS(profile domain.MerchantTaxProfile, in VATOSSInput) domain.TaxForm {
	totalVAT := int64(0)
	totalNet := int64(0)
	for _, v := range in.ByCountry {
		totalVAT += v.VATAmount
		totalNet += v.NetSales
	}
	formID := fmt.Sprintf("vatoss-%d-%02d-%s", in.Year, in.Month, in.MerchantID)
	return domain.TaxForm{
		FormID:      formID,
		FormType:    domain.FormVATOSS,
		MerchantID:  profile.MerchantID,
		Year:        in.Year,
		Jurisdiction: "EU",
		GrossAmount: totalNet + totalVAT,
		Currency:    "EUR",
		Status:      domain.FormReady,
		GeneratedAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"merchant_id":     profile.MerchantID,
			"vat_number":      profile.VAT_Number,
			"home_country":    in.HomeCountry,
			"reporting_year":  in.Year,
			"reporting_month": in.Month,
			"total_net":       dollars(totalNet),  // EUR cents → 单位
			"total_vat":       dollars(totalVAT),
			"by_country":      vatBreakdown(in.ByCountry),
		},
	}
}

func vatBreakdown(m map[string]VATCountryItem) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(m))
	for k, v := range m {
		out = append(out, map[string]interface{}{
			"country":    k,
			"net":        dollars(v.NetSales),
			"vat_rate":   v.VATRate,
			"vat_amount": dollars(v.VATAmount),
		})
	}
	return out
}

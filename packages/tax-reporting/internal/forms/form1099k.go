// Package forms — 生成各税表的 payload.
//
// 这里产 JSON payload (符合 IRS FIRE 格式); PDF 渲染交给独立 PDF 服务 (gotenberg / weasyprint).
//
// IRS 1099-K format (FIRE Publication 1220, 2024 update):
//   - Issuer Name = 平台法人
//   - Payee = 商户 (legal name + TIN)
//   - Box 1a Gross amount of payment card / third-party network transactions
//   - Box 1b Card-not-present (CNP) transactions (subset of 1a)
//   - Box 3 Number of payment transactions
//   - Box 5a-5l 月份明细 (Jan..Dec gross)
//   - Box 6 State (商户 state)
//   - Box 7 State identification number

package forms

import (
	"fmt"
	"time"

	"reconcile-system/packages/tax-reporting/internal/domain"
)

// Filer 平台自身税务信息 (出 1099-K 的"发行人")
type Filer struct {
	Name    string
	TIN     string // platform EIN
	Address domain.Address
}

// Generate1099K 给一个 merchant 的年度 aggregate 出 1099-K payload.
// 返回的 TaxForm.Payload 可直接转 IRS FIRE record.
func Generate1099K(filer Filer, profile domain.MerchantTaxProfile, agg domain.Aggregate) domain.TaxForm {
	box1a := agg.TotalGross
	// box1b: CNP — 这里我们假设全是 CNP (互联网商户). 真实需要 channel 区分:
	//   visa_card_present / mc_card_present 分一份, online 分另一份.
	box1b := agg.TotalGross
	if cp, ok := agg.ByChannel["card_present"]; ok {
		box1b = agg.TotalGross - cp
	}

	monthlyDollar := make([]float64, 12)
	for i, m := range agg.MonthlyGross {
		monthlyDollar[i] = float64(m) / 100.0
	}

	formID := fmt.Sprintf("1099k-%d-%s", agg.Year, agg.MerchantID)
	return domain.TaxForm{
		FormID:       formID,
		FormType:     domain.Form1099K,
		MerchantID:   agg.MerchantID,
		Year:         agg.Year,
		Jurisdiction: "US",
		GrossAmount:  agg.TotalGross,
		Currency:     "USD",
		Status:       domain.FormReady,
		GeneratedAt:  time.Now().UTC(),
		Payload: map[string]interface{}{
			"filer": map[string]interface{}{
				"name":    filer.Name,
				"tin":     filer.TIN,
				"address": filer.Address,
			},
			"payee": map[string]interface{}{
				"merchant_id":   profile.MerchantID,
				"legal_name":    profile.LegalName,
				"tin":           profile.TIN,
				"tin_type":      profile.TINType,
				"address":       profile.BusinessAddr,
				"tax_class":     profile.TaxClass,
			},
			"year":      agg.Year,
			"box_1a_gross":          dollars(box1a),
			"box_1b_card_not_present": dollars(box1b),
			"box_3_txn_count":       agg.TotalCount,
			"box_5_monthly_gross":   monthlyDollar,
			"box_6_state":           profile.BusinessAddr.State,
		},
	}
}

func dollars(cents int64) float64 {
	return float64(cents) / 100.0
}

// w9w8.go — W-9 (US) / W-8 (foreign) 数据收集 + 校验.
//
// 商户入网时 (KYB) 走到这里:
//   - country=US → 收 W-9: legal_name + tin + tax_class + address + 签名
//   - country≠US, 个人 → W-8BEN
//   - country≠US, 公司 → W-8BEN-E
//
// 我们不渲染 PDF (走外部 PDF 服务); 只产 payload + 决定 form_type.

package forms

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"reconcile-system/packages/tax-reporting/internal/domain"
)

var (
	ErrMissingTIN = errors.New("w-9: TIN required")
	ErrMissingName = errors.New("w9/w8: legal name required")
	ErrMissingCountry = errors.New("w-8: country required")
)

// W9Submit 商户提交 W-9; 校验 + 标记 profile.
func W9Submit(p *domain.MerchantTaxProfile, tin string, tinType domain.TINType) error {
	if p.LegalName == "" {
		return ErrMissingName
	}
	tin = strings.ReplaceAll(strings.TrimSpace(tin), "-", "")
	if tin == "" {
		return ErrMissingTIN
	}
	// EIN: 9 位; SSN: 9 位 (XXX-XX-XXXX); ITIN: 9 位 9xx-7x/8x-xxxx
	if len(tin) != 9 {
		return errors.New("w-9: TIN must be 9 digits")
	}
	for _, c := range tin {
		if c < '0' || c > '9' {
			return errors.New("w-9: TIN must be digits")
		}
	}
	p.TIN = tin
	p.TINType = tinType
	p.TIN_Hash = sha16(tin)
	p.W9_Submitted = true
	p.W9_Date = time.Now().UTC()
	return nil
}

// W8Submit 非 US 商户; w8Type = "W-8BEN" (个人) / "W-8BEN-E" (公司).
func W8Submit(p *domain.MerchantTaxProfile, w8Type string, country string) error {
	if p.LegalName == "" {
		return ErrMissingName
	}
	if country == "" || country == "US" {
		return ErrMissingCountry
	}
	if w8Type != "W-8BEN" && w8Type != "W-8BEN-E" && w8Type != "W-8ECI" && w8Type != "W-8IMY" {
		return errors.New("w-8: unsupported form type")
	}
	p.W8_Submitted = true
	p.W8_Date = time.Now().UTC()
	p.W8_Type = w8Type
	p.Country = country
	p.TINType = domain.TINTypeForeign
	return nil
}

// GenerateW9Form 产 W-9 confirmation document.
func GenerateW9Form(profile domain.MerchantTaxProfile) domain.TaxForm {
	return domain.TaxForm{
		FormID:      "w9-" + profile.MerchantID,
		FormType:    domain.FormW9,
		MerchantID:  profile.MerchantID,
		Jurisdiction: "US",
		Status:      domain.FormReady,
		GeneratedAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"legal_name":   profile.LegalName,
			"tin_hash":     profile.TIN_Hash, // 不放明文
			"tin_type":     profile.TINType,
			"tax_class":    profile.TaxClass,
			"address":      profile.BusinessAddr,
			"submitted_at": profile.W9_Date,
		},
	}
}

func GenerateW8Form(profile domain.MerchantTaxProfile) domain.TaxForm {
	formType := domain.FormW8BEN
	if profile.W8_Type == "W-8BEN-E" {
		formType = domain.FormW8BENE
	}
	return domain.TaxForm{
		FormID:      "w8-" + profile.MerchantID,
		FormType:    formType,
		MerchantID:  profile.MerchantID,
		Jurisdiction: "intl",
		Status:      domain.FormReady,
		GeneratedAt: time.Now().UTC(),
		Payload: map[string]interface{}{
			"legal_name":   profile.LegalName,
			"country":      profile.Country,
			"w8_type":      profile.W8_Type,
			"address":      profile.BusinessAddr,
			"submitted_at": profile.W8_Date,
		},
	}
}

func sha16(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}

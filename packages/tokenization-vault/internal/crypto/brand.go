// brand.go — 从 PAN 推卡组 (Visa / Mastercard / Amex …).
//
// 真生产应该用完整 BIN range 数据库 (SkyMobile / Bin-IQ); 这里只做粗判:

package crypto

import "strings"

// CardBrand 跟 domain.CardBrand 对齐 (string 兼容).
const (
	BrandVisa       = "visa"
	BrandMastercard = "mastercard"
	BrandAmex       = "amex"
	BrandDiscover   = "discover"
	BrandJCB        = "jcb"
	BrandUnionPay   = "unionpay"
	BrandUnknown    = "unknown"
)

// DetectBrand 用 PAN 前缀粗判.
func DetectBrand(pan string) string {
	if len(pan) < 4 {
		return BrandUnknown
	}
	p := strings.TrimSpace(pan)
	switch {
	case p[0] == '4':
		return BrandVisa
	case isMastercardPrefix(p):
		return BrandMastercard
	case p[:2] == "34" || p[:2] == "37":
		return BrandAmex
	case p[:4] == "6011" || p[:2] == "65":
		return BrandDiscover
	case p[:4] == "3528" || p[:4] == "3589":
		return BrandJCB
	case p[:2] == "62":
		return BrandUnionPay
	}
	return BrandUnknown
}

// Mastercard: 51-55 旧版, 2221-2720 新增 (2-series, since 2016)
func isMastercardPrefix(p string) bool {
	if len(p) >= 2 {
		if p[0] == '5' && p[1] >= '1' && p[1] <= '5' {
			return true
		}
	}
	if len(p) >= 4 {
		n := 0
		for i := 0; i < 4; i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n >= 2221 && n <= 2720 {
			return true
		}
	}
	return false
}

// Luhn 校验位检查 — exchange 前快速 reject 无效 PAN.
func ValidLuhn(pan string) bool {
	sum := 0
	alt := false
	for i := len(pan) - 1; i >= 0; i-- {
		c := pan[i]
		if c < '0' || c > '9' {
			return false
		}
		n := int(c - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0 && sum > 0
}

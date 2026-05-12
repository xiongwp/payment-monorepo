// dispatcher.go — Network → Generate 派发.
//
// 上层 (service/payout.go) 调:
//   payload, err := rails.Generate(orig, payouts, accounts, rails.NetworkSEPA)
//
// 不同 network 调不同生成器, 屏蔽实现细节.

package rails

import (
	"fmt"
	"strings"
)

// Generate 单一入口.
func Generate(orig Originator, payouts []Payout, accounts []BankAccount, network Network) ([]byte, error) {
	if len(payouts) == 0 {
		return nil, fmt.Errorf("rails: no payouts")
	}
	switch network {
	case NetworkACH:
		return GenerateNACHA(orig, payouts, accounts, "A")
	case NetworkSEPA:
		return GenerateSEPA(orig, payouts, accounts)
	case NetworkWireSWIFT:
		// MT103 是单笔 — 这里展平
		if len(payouts) == 1 {
			return GenerateMT103(orig, payouts[0], accounts[0], ChargeSHA)
		}
		// 多笔: 逐笔生成 + 分隔符 (实际生产应每笔单独 SWIFT 发送)
		return generateMT103Batch(orig, payouts, accounts)
	}
	return nil, fmt.Errorf("rails: unsupported network %q", network)
}

func generateMT103Batch(orig Originator, payouts []Payout, accounts []BankAccount) ([]byte, error) {
	out := []byte{}
	for i := range payouts {
		b, err := GenerateMT103(orig, payouts[i], accounts[i], ChargeSHA)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out, nil
}

// SelectNetworkFor 简单路由: 根据收款方国家 / 货币 决定走哪条 rail.
//
// 真生产更复杂 (考虑成本 / 速度 / cutoff time / 商户偏好).
func SelectNetworkFor(currency, beneficiaryCountry string) Network {
	cur := strings.ToUpper(currency)
	c := strings.ToUpper(beneficiaryCountry)
	switch {
	case cur == "EUR" && isSEPACountry(c):
		return NetworkSEPA
	case cur == "USD" && c == "US":
		return NetworkACH
	case cur == "GBP" && c == "GB":
		return NetworkUKFaster
	}
	// 默认跨境
	return NetworkWireSWIFT
}

func isSEPACountry(c string) bool {
	// SEPA 36 国 — 简化列举 EU 27 + EEA/EFTA 4 + 5 个微国家
	sepa := map[string]bool{
		"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true,
		"DK": true, "EE": true, "FI": true, "FR": true, "DE": true, "GR": true,
		"HU": true, "IE": true, "IT": true, "LV": true, "LT": true, "LU": true,
		"MT": true, "NL": true, "PL": true, "PT": true, "RO": true, "SK": true,
		"SI": true, "ES": true, "SE": true,
		"IS": true, "LI": true, "NO": true, "CH": true,
		"MC": true, "SM": true, "VA": true, "AD": true, "GB": true, // GB still in SEPA post-Brexit
	}
	return sepa[c]
}

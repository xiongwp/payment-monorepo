package channel

import "strings"

// 与 order-core 约定的规范失败码。payment-channel 自己会做一次归一，payment-core
// 再兜底一次（防止 adapter 漏写）。
const (
	FailCardDeclined       = "card_declined"
	FailInsufficientFunds  = "insufficient_funds"
	FailRiskBlocked        = "risk_blocked"
	FailAuthFailed         = "auth_failed"
	FailExpired            = "expired"
	FailChannelUnavailable = "channel_unavailable"
	FailUnknown            = "unknown"
)

// NormalizeFailure 如果输入已经是规范码则原样返回，否则按关键词兜底。
func NormalizeFailure(code string) string {
	switch code {
	case FailCardDeclined, FailInsufficientFunds, FailRiskBlocked,
		FailAuthFailed, FailExpired, FailChannelUnavailable, FailUnknown:
		return code
	}
	k := strings.ToUpper(code)
	switch {
	case strings.Contains(k, "INSUFFICIENT"), strings.Contains(k, "NSF"):
		return FailInsufficientFunds
	case strings.Contains(k, "RISK"), strings.Contains(k, "FRAUD"):
		return FailRiskBlocked
	case strings.Contains(k, "EXPIRED"), strings.Contains(k, "TIMEOUT"):
		return FailExpired
	case strings.Contains(k, "AUTH"), strings.Contains(k, "OTP"), strings.Contains(k, "3DS"):
		return FailAuthFailed
	case strings.Contains(k, "UNAVAILABLE"), strings.Contains(k, "MAINTENANCE"):
		return FailChannelUnavailable
	case strings.Contains(k, "DECLINED"), strings.Contains(k, "REFUSED"), strings.Contains(k, "REJECTED"):
		return FailCardDeclined
	case code == "":
		return FailUnknown
	}
	return FailUnknown
}

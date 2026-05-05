//go:build ignore
// +build ignore

package channel

import "strings"

// 归一失败码集合，与 order-core/docs/PAYMENT_CORE_INTEGRATION.md §5 对齐。
const (
	FailCardDeclined       = "card_declined"
	FailInsufficientFunds  = "insufficient_funds"
	FailRiskBlocked        = "risk_blocked"
	FailAuthFailed         = "auth_failed"
	FailExpired            = "expired"
	FailChannelUnavailable = "channel_unavailable"
	FailUnknown            = "unknown"
)

// MapFailure 把渠道原始码 / 错误消息归一。每个 adapter 可以在自己包里
// 先做一次更精细的映射再 fallback 到这里。
func MapFailure(adapter, raw string) string {
	k := strings.ToUpper(raw)
	switch {
	case contains(k, "INSUFFICIENT", "BALANCE_NOT_ENOUGH", "NSF"):
		return FailInsufficientFunds
	case contains(k, "RISK", "FRAUD"):
		return FailRiskBlocked
	case contains(k, "EXPIRED", "TIMEOUT", "TIME_OUT"):
		return FailExpired
	case contains(k, "AUTH_FAILED", "INVALID_OTP", "INVALID_PASSWORD", "3DS"):
		return FailAuthFailed
	case contains(k, "UNAVAILABLE", "SYSTEM_ERROR", "MAINTENANCE", "RATE_LIMIT"):
		return FailChannelUnavailable
	case contains(k, "DECLINED", "REFUSED", "REJECTED"):
		return FailCardDeclined
	case raw == "":
		return FailUnknown
	}
	return FailUnknown
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
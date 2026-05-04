// redact.go: PII 字段脱敏 helper。
//
// 用在 zap log / audit 序列化 / metrics labels 任何会写到落盘 / 发出去的
// 路径上，避免卡号 / 邮箱 / 手机 / 身份证号被原样持久化。GDPR + PCI-DSS
// 合规基线。
//
// 设计：
//   - MaskCardNumber("4111111111111111") → "411111******1234"  (BIN + last4)
//   - MaskEmail("alice@example.com") → "a****@example.com"
//   - MaskPhone("+8613800138000")    → "+8613****8000"
//   - MaskIDNumber("110101199001011234") → "110101**********34" (中国身份证)
//
// 全部 ASCII-safe，不 panic 任何输入；空字符串 / 短字符串原样返回（短到
// mask 后没意义）。
package safety

import "strings"

// MaskCardNumber 卡号脱敏：保留 BIN（前 6）+ last4，中间星号填充。
// 长度 < 10 视作不合法卡号，直接返 "***"（避免泄露任何前缀）。
//
// 接 PCI-DSS 要求：日志 / audit 不允许存完整卡号；但 BIN + last4 是
// "可识别" 但 "不可还原" 的级别，PCI 允许。
func MaskCardNumber(s string) string {
	s = stripNonDigits(s)
	if len(s) < 10 {
		return "***"
	}
	if len(s) <= 10 {
		// 极短卡 (e.g. 11 chars)：保守只留 last4
		return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
	}
	mid := len(s) - 6 - 4
	if mid < 4 {
		mid = 4
	}
	return s[:6] + strings.Repeat("*", mid) + s[len(s)-4:]
}

// MaskEmail 保留首字母 + 完整域名。
// "alice@example.com" → "a****@example.com"
// 没 @ 的不当 email 处理 → 整体 mask。
func MaskEmail(s string) string {
	at := strings.LastIndex(s, "@")
	if at <= 0 {
		return MaskGeneric(s)
	}
	user := s[:at]
	domain := s[at:]
	if len(user) <= 1 {
		return user + strings.Repeat("*", 4) + domain
	}
	return user[:1] + strings.Repeat("*", 4) + domain
}

// MaskPhone 保留国际号前缀 + last4，中间 mask。
// "+8613800138000" → "+8613****8000"
// "13800138000"     → "138****8000"
func MaskPhone(s string) string {
	digits := stripNonDigits(s)
	if len(digits) < 7 {
		return MaskGeneric(s)
	}
	prefix := ""
	if strings.HasPrefix(s, "+") {
		// 保留国际号前缀（"+86" + 第一位区号）
		// 简化：保留前 4 字符 ("+861")
		if len(s) >= 4 {
			prefix = s[:4]
		}
	} else {
		prefix = digits[:3]
	}
	last4 := digits[len(digits)-4:]
	return prefix + "****" + last4
}

// MaskIDNumber 中国 18 位身份证 / 通用 ID number 脱敏。
// 保留前 6 位（含地区码）+ last 2，中间全 mask。
// 短于 10 → 整体 mask。
func MaskIDNumber(s string) string {
	if len(s) < 10 {
		return MaskGeneric(s)
	}
	mid := len(s) - 6 - 2
	if mid < 4 {
		mid = 4
	}
	return s[:6] + strings.Repeat("*", mid) + s[len(s)-2:]
}

// MaskIPv4 IP 脱敏：保留前两段，后两段 mask。
// "192.168.1.1" → "192.168.*.*"
func MaskIPv4(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return MaskGeneric(s)
	}
	return parts[0] + "." + parts[1] + ".*.*"
}

// MaskGeneric 通用兜底：前 1 字符 + 星号填充。极短字符串原样返回。
func MaskGeneric(s string) string {
	switch {
	case len(s) <= 2:
		return s
	case len(s) <= 6:
		return s[:1] + strings.Repeat("*", len(s)-1)
	default:
		return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
	}
}

func stripNonDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

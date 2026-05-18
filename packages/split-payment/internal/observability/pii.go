// pii.go — SP-AC-7 PROD3: PII 脱敏 helper.
//
// 应对监管: 日志里不应出现完整 PAN / 护照号 / 身份证号 / 完整账户号 / 完整邮箱.
// 一般做法是日志写出前调 redact, 把敏感字段中间段替换成 ***.
//
// 当前实现是 dummy 化 helper, 调用方显式调 Mask*(); 高级方案是写个 zap encoder
// 拦截 sensitive field names 自动 mask, 但 zap 不容易 hook field name.
package observability

import (
	"regexp"
	"strings"
)

var (
	// 留头 6 尾 4, 中间 ***. 适合 PAN (16 位卡号), bankAcct.
	maskMiddleRe = regexp.MustCompile(`^(.{0,6})(.*?)(.{0,4})$`)

	// 邮箱 user@domain → u***@domain
	emailRe = regexp.MustCompile(`^([^@])([^@]*)(@.+)$`)
)

// MaskAccountNo 账户号 / PAN 通用脱敏: 留头 6 尾 4.
// 入参短 (< 10 位) 时只留头 1 尾 1, 防全暴露.
//
//   "608010000011200000" → "608010**********0000"
//   "4242424242424242"   → "424242******4242"
func MaskAccountNo(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 4 {
		return "****"
	}
	if len(s) <= 10 {
		return string(s[0]) + strings.Repeat("*", len(s)-2) + string(s[len(s)-1])
	}
	return s[:6] + strings.Repeat("*", len(s)-10) + s[len(s)-4:]
}

// MaskEmail "alice@example.com" → "a****@example.com".
func MaskEmail(s string) string {
	m := emailRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	return m[1] + strings.Repeat("*", max1(len(m[2]), 4)) + m[3]
}

// MaskPhone "+8613800138000" → "+8613***8000". 简单中间替换.
func MaskPhone(s string) string {
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:4] + strings.Repeat("*", len(s)-8) + s[len(s)-4:]
}

// MaskName "张三" → "张**".  英文名留首字母.
func MaskName(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) == 1 {
		return "*"
	}
	return string(r[0]) + strings.Repeat("*", len(r)-1)
}

func max1(a, b int) int {
	if a > b {
		return a
	}
	return b
}

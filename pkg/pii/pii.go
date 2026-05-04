// Package pii: PII 字段脱敏工具，用于结构化日志 / 错误返回。
//
// 原则：脱敏后的字符串保留**少量识别上下文**让运维能区分两条不同的记录，
// 同时让原文不可恢复。任何字段长度 < 阈值时返回固定占位（"***"）避免泄露
// 短字段全文。
//
// 不做哈希：哈希在长生命周期日志里仍然可被字典攻击恢复（手机号空间 < 10^11，
// 几小时即可暴力哈希反查）。脱敏 > 哈希。
package pii

import "strings"

// MaskEmail 邮箱脱敏。规则：
//
//	a@b.com         → ***@b.com
//	ab@b.com        → a*@b.com
//	abcdef@b.com    → a***f@b.com
//	格式不合法（无 @）→ "***"
func MaskEmail(s string) string {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return "***"
	}
	local, domain := s[:at], s[at:]
	switch {
	case len(local) <= 1:
		return "***" + domain
	case len(local) <= 2:
		return string(local[0]) + "*" + domain
	default:
		return string(local[0]) + "***" + string(local[len(local)-1]) + domain
	}
}

// MaskPhone 手机号脱敏。保留前 3 + 后 4 位（国际号 + 末段足够区分），中间星号。
//
//	+639171234567  → +63*****4567
//	09171234567    → 091*****4567
//	长度 < 7 → "***"
func MaskPhone(s string) string {
	// 把空格和分隔符删掉再脱敏，避免输入差异导致脱敏结果不稳定
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '-' || c == '(' || c == ')' {
			continue
		}
		b.WriteByte(c)
	}
	clean := b.String()
	if len(clean) < 7 {
		return "***"
	}
	return clean[:3] + strings.Repeat("*", len(clean)-7) + clean[len(clean)-4:]
}

// MaskTail 通用尾部脱敏：保留尾 N 字符，前面 *。短于 N+1 时返回 "***"。
// 用于 ID card / passport / API key 之类。
func MaskTail(s string, keep int) string {
	if keep <= 0 || len(s) <= keep+1 {
		return "***"
	}
	return strings.Repeat("*", len(s)-keep) + s[len(s)-keep:]
}

// risk.go：卡支付风险判定辅助。各 network adapter 在响应到手时统一过这层，
// 把 AVS / CVV / 3DS / fraud_score / decline_code 归一成本地决策。
//
// 设计原则：
//   - **不在 adapter 内做交易拒/通的最终决定**——final 决策走 risk-manage 服务
//     （adapter 只把卡组织返回的字段如实抬回去，加结构化 enum）
//   - 但 adapter 必须拒绝**明显异常**：网络明确 declined / 防欺诈强标识
//   - HIGH_RISK_DECLINE / SUSPECTED_FRAUD 等卡组织级 hard decline 必须直接拒，
//     不让上层 risk 误判为"通过 + 待人工"
package httpx

import "strings"

// RiskAssessment 把卡组织响应中和风险相关的字段聚合，给 caller 用。
// 每家 network 自己 map 字段到这个结构。
type RiskAssessment struct {
	// AVSResult AVS 结果。Y=完全匹配 / A=地址匹配但邮编不匹配 / N=不匹配 /
	// U=数据不可用。空字符串 = 未跑 AVS（CNP 卡 / 商户没传地址）。
	AVSResult string

	// CVVResult CVV/CVC 结果。M=匹配 / N=不匹配 / P=未处理 / U=不支持。
	CVVResult string

	// ThreeDSStatus 3DS 状态。Y=认证通过 / A=尝试 / N=失败 / U=不支持。
	// 与 ThreeDSEci 配合：ECI 02/05 = 完整认证；ECI 01/06 = 尝试认证。
	ThreeDSStatus string
	ThreeDSEci    string

	// FraudScore 卡组织端反欺诈评分（0-100，越高越可疑）。各家阈值不一：
	// Visa CyberSource Decision Manager: > 60 高危；MIP MFA: > 70 高危。
	FraudScore int

	// DeclineCategory 卡组织级 decline 原因归一。HARD = 永久拒（盗卡 / 黑名单），
	// SOFT = 可重试（限额 / 余额）。
	DeclineCategory string // HARD / SOFT / ""(approved)

	// RawDeclineCode 卡组织原始 code，留给 audit / 客服。
	RawDeclineCode string
}

// IsHardDecline 网络明确 hard decline，禁止 retry。
func (r *RiskAssessment) IsHardDecline() bool {
	return r != nil && r.DeclineCategory == "HARD"
}

// IsHighRisk 风控相关字段给 caller 一个轻量 bool。
//   - AVS 不匹配
//   - CVV 不匹配
//   - 3DS 失败
//   - FraudScore > 60
// 满足任一即视为高风险，caller（processor）可以选择不入账或 escalate 人工。
func (r *RiskAssessment) IsHighRisk() bool {
	if r == nil {
		return false
	}
	if r.AVSResult == "N" || r.CVVResult == "N" || r.ThreeDSStatus == "N" {
		return true
	}
	if r.FraudScore >= 60 {
		return true
	}
	return false
}

// MapDeclineCode 各 network 共用的 decline 分类表。
//
// 凡是返回 HARD 的，processor.Authorize 必须 status=declined 不再重试，且
// 上游 risk-manage 应记入 black-listed token / user 候选名单（48h 观察）。
// SOFT 的允许商户提示用户重试（金额超限 / 余额不足 / 卡失效）。
func MapDeclineCode(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	switch c {
	case "":
		return ""
	// Visa / MC 通用 hard
	case "FRAUD", "STOLEN_CARD", "LOST_CARD", "PICK_UP_CARD",
		"FRAUDULENT_TRANSACTION", "REVOKED_AUTHORIZATION",
		"DO_NOT_HONOR", "CARD_BLOCKED", "BLACKLIST",
		"HIGH_RISK_DECLINE", "SUSPECTED_FRAUD",
		"R0", "R1", "R3":
		return "HARD"
	// 软拒可重试
	case "INSUFFICIENT_FUNDS", "EXCEEDS_AMOUNT_LIMIT", "EXCEEDS_FREQUENCY_LIMIT",
		"EXPIRED_CARD", "INVALID_CARD",
		"ISSUER_NOT_AVAILABLE", "TRY_AGAIN_LATER",
		"PROCESSING_ERROR", "TIMEOUT":
		return "SOFT"
	}
	// 未识别 code 默认归 SOFT，让 caller 谨慎重试 1 次
	return "SOFT"
}

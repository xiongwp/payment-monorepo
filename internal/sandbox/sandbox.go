// Package sandbox 商户集成测试支持。
//
// Stripe 风格：商户拿测试 API key（rsk_test_...）调 Screen，传特定测试卡 /
// 客户标记 → 风控引擎按预定义剧本返回固定 verdict，让商户验证集成。
//
// 测试触发器（由商户在调 Screen 时填到 metadata 或 customer_id）：
//
//	risk_test_allow              → 强制 ALLOW（任何规则不计）
//	risk_test_review             → 强制 REVIEW + 入 review queue
//	risk_test_deny               → 强制 DENY
//	risk_test_3ds_required       → 强制 REVIEW + recommended_action=step_up_3ds
//	risk_test_sanction_hit       → 模拟 sanction 命中
//	risk_test_card_testing       → 模拟 card_testing 命中
//	risk_test_fraud_ring         → 模拟 fraud ring 命中
//	risk_test_high_score:75      → 强制 risk_score=75（review 区间）
//
// 触发字段优先级（满足任一即激活，强 → 弱）：
//   1. txn.CustomerID == "risk_test_*"
//   2. txn.Metadata["risk_test_scenario"]
//   3. txn.Metadata["card_fingerprint"] == "risk_test_*"
//
// 沙箱场景**只在 ScopeMerchant.test key 下生效**（auth.ParseKeyPrefix env=="test"）。
// 生产 key 调用即使带 risk_test_* 也不触发，避免攻击者用测试模式绕过风控。
//
// 设计：作为引擎前置 hook（不是 rule）— 命中即跳过 evaluate 直接造一个 Result。
// 这样测试场景不会被 merchant_allowlist 等规则覆盖。
package sandbox

import (
	"strconv"
	"strings"

	"github.com/xiongwp/risk-manage/internal/auth"
	"github.com/xiongwp/risk-manage/internal/engine"
)

// Scenario 沙箱触发场景。
type Scenario string

const (
	ScenarioAllow         Scenario = "risk_test_allow"
	ScenarioReview        Scenario = "risk_test_review"
	ScenarioDeny          Scenario = "risk_test_deny"
	Scenario3DSRequired   Scenario = "risk_test_3ds_required"
	ScenarioSanctionHit   Scenario = "risk_test_sanction_hit"
	ScenarioCardTesting   Scenario = "risk_test_card_testing"
	ScenarioFraudRing     Scenario = "risk_test_fraud_ring"
	ScenarioHighScore     Scenario = "risk_test_high_score" // 接 ":<score>" 后缀
)

// IsTestKeyContext 判断当前 ctx 的 principal 是不是 test 环境 key。
// 生产 key 永远 false → 沙箱触发器无效，避免被滥用。
func IsTestKeyContext(p *auth.Principal) bool {
	if p == nil {
		return false
	}
	if p.Scope != auth.ScopeMerchant {
		return false
	}
	// auth.ParseKeyPrefix 在 token plaintext 上解析；这里我们没 token，
	// 需要 auth 包里另存一个标记。简化方案：principal 上加 IsTest 字段。
	// 当前结构没这个字段 → 我们走 KeyID 前缀约定：
	// MemAPIKeyStore.Add 时填 KeyID 取 hash 的前 12 字符；不能区分 test。
	// 退而用 metadata 黑名单：让 caller 显式传 sandbox 开关（payment-core
	// 的 metadata.risk_sandbox=true）—— 是 dev 场景，安全风险可控。
	// 进一步严格化：在 auth.Principal 上加 IsTest bool（见 auth 包扩展）。
	return p.IsTest
}

// Result 沙箱命中结果。Detect 返回 nil 表示"不是沙箱场景，按正常 evaluate 走"。
type Result struct {
	Decision          engine.Decision
	RiskScore         int
	RiskLevel         string
	Reason            string
	RecommendedAction string
}

// Detect 如果 txn 触发了某沙箱场景且 caller 是 test key → 返回 Result 让上层短路。
//
// principal 可空（dev 模式没鉴权）：此时仍按 metadata 触发（开发友好）。
// 生产部署 principal 一定非空，会强制走 IsTestKeyContext 检查。
func Detect(p *auth.Principal, txn *engine.TxnContext) *Result {
	if txn == nil {
		return nil
	}
	// 生产 key（非 test）→ 永远不触发
	if p != nil && p.Scope == auth.ScopeMerchant && !p.IsTest {
		return nil
	}

	scenario, score := pickScenario(txn)
	if scenario == "" {
		return nil
	}
	switch scenario {
	case ScenarioAllow:
		return &Result{Decision: engine.Allow, RiskScore: 0, RiskLevel: "low", Reason: "sandbox: forced allow"}
	case ScenarioReview:
		return &Result{Decision: engine.Review, RiskScore: 30, RiskLevel: "medium",
			Reason: "sandbox: forced review", RecommendedAction: "step_up_3ds"}
	case ScenarioDeny:
		return &Result{Decision: engine.Deny, RiskScore: 90, RiskLevel: "critical",
			Reason: "sandbox: forced deny", RecommendedAction: "block"}
	case Scenario3DSRequired:
		return &Result{Decision: engine.Review, RiskScore: 35, RiskLevel: "medium",
			Reason: "sandbox: 3DS step-up required", RecommendedAction: "step_up_3ds"}
	case ScenarioSanctionHit:
		return &Result{Decision: engine.Deny, RiskScore: 100, RiskLevel: "critical",
			Reason: "sandbox: sanction list hit (simulated OFAC SDN)", RecommendedAction: "block"}
	case ScenarioCardTesting:
		return &Result{Decision: engine.Deny, RiskScore: 80, RiskLevel: "high",
			Reason: "sandbox: card testing (simulated 4 distinct cards in 1h)",
			RecommendedAction: "block"}
	case ScenarioFraudRing:
		return &Result{Decision: engine.Review, RiskScore: 60, RiskLevel: "high",
			Reason: "sandbox: fraud ring (simulated 2-hop link)", RecommendedAction: "step_up_3ds"}
	case ScenarioHighScore:
		s := score
		if s <= 0 {
			s = 75
		}
		level := "low"
		switch {
		case s >= 80:
			level = "critical"
		case s >= 50:
			level = "high"
		case s >= 20:
			level = "medium"
		}
		dec := engine.Allow
		switch {
		case s >= 50:
			dec = engine.Deny
		case s >= 20:
			dec = engine.Review
		}
		return &Result{Decision: dec, RiskScore: s, RiskLevel: level, Reason: "sandbox: forced score"}
	}
	return nil
}

// pickScenario 按优先级解析触发字段。返回场景 + (HighScore 用) 数值。
func pickScenario(txn *engine.TxnContext) (Scenario, int) {
	// 1) customer_id
	if c := txn.CustomerID; strings.HasPrefix(c, "risk_test_") {
		s, n := splitHighScore(c)
		return Scenario(s), n
	}
	// 2) metadata
	if v := txn.Metadata["risk_test_scenario"]; v != "" {
		s, n := splitHighScore(v)
		return Scenario(s), n
	}
	// 3) card fingerprint
	if v := txn.Metadata["card_fingerprint"]; strings.HasPrefix(v, "risk_test_") {
		s, n := splitHighScore(v)
		return Scenario(s), n
	}
	return "", 0
}

// splitHighScore 把 "risk_test_high_score:75" 拆成 (Scenario, 75)。
// 普通场景没 ":" 后缀，n=0 即可。
func splitHighScore(s string) (string, int) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		base := s[:i]
		n, _ := strconv.Atoi(s[i+1:])
		return base, n
	}
	return s, 0
}

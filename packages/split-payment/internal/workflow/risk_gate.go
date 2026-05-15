// risk_gate.go — SP-3B Risk + AML gate.
//
// 资金流执行前的两道闸:
//
//   1. RiskGate  (调 risk-manage 服务) — 风控决策 allow/review/deny
//   2. AMLGate   (调 aml-screening 服务) — 大额 / 高风险地区 trigger 反洗钱
//
// 设计:
//   - 抽象接口 RiskClient / AMLClient, 由 caller (cmd/server/main.go) 注入真实 gRPC 实现.
//     单测 / dev 用 AlwaysAllowRisk / AlwaysAllowAML 占位.
//   - Engine.executeOne 在 Translate 之后, accounting.Post 之前调 evaluateRisk(plan):
//       deny     → plan.Status="rejected" + 发 flow.rejected 事件, 不再执行
//       review   → plan.Status="awaiting_review", 等人工放行 (Phase 3D 加 admin UI)
//       allow    → 继续
//   - AML 仅对超阈金额 / 跨境 / 高风险国家触发 (本地阈值 default $10K)
//
// 失败 fail-safe 策略:
//   - 风控服务不可达 (network err) → 默认 fail-closed (reject)
//   - 业务事件 (e.g. hold.expired) 不走风控 (内部驱动, 已经验过)
package workflow

import (
	"context"
	"errors"

	"reconcile-system/packages/split-payment/internal/domain"

	"go.uber.org/zap"
)

// RiskDecision 风控决策结果.
type RiskDecision string

const (
	RiskAllow  RiskDecision = "allow"
	RiskReview RiskDecision = "review" // 等人工
	RiskDeny   RiskDecision = "deny"
)

// RiskRequest 给 risk-manage 的入参.
type RiskRequest struct {
	Event       string            // charge.succeeded / refund.completed / hold.expired
	ChargeID    string
	MerchantID  string
	AmountMinor int64
	Currency    string
	Attributes  map[string]string
	GraphKey    string
}

// RiskResult 返回.
type RiskResult struct {
	Decision  RiskDecision
	Score     int    // 0-100, 越高风险越大 (allow≤30, review 30-70, deny>70)
	RuleID    string // 命中的规则
	Reason    string // 人类可读
}

// RiskClient 调 risk-manage 服务 (gRPC) 的抽象.
//
// 真实实现 build 时由 main.go 注入: e.g. NewGRPCRiskClient(connToRiskService).
// dev 用 AlwaysAllowRisk{} 跳过风控.
type RiskClient interface {
	Evaluate(ctx context.Context, req RiskRequest) (*RiskResult, error)
}

// AMLClient 调 aml-screening 服务的抽象.
type AMLClient interface {
	Screen(ctx context.Context, req RiskRequest) (*RiskResult, error)
}

// AlwaysAllowRisk dev / 单测.
type AlwaysAllowRisk struct{}

// Evaluate.
func (AlwaysAllowRisk) Evaluate(_ context.Context, _ RiskRequest) (*RiskResult, error) {
	return &RiskResult{Decision: RiskAllow, Score: 0, Reason: "no risk client configured"}, nil
}

// AlwaysAllowAML dev / 单测.
type AlwaysAllowAML struct{}

// Screen.
func (AlwaysAllowAML) Screen(_ context.Context, _ RiskRequest) (*RiskResult, error) {
	return &RiskResult{Decision: RiskAllow, Score: 0, Reason: "no aml client configured"}, nil
}

// RiskGateConfig 阈值与 fail-safe 策略.
type RiskGateConfig struct {
	// AMLThresholdMinor 超过此金额走 AML screen (默认 $10000 = 1_000_000 cents)
	AMLThresholdMinor int64

	// SkipEvents 不走风控的事件列表 (内部驱动 e.g. hold.expired)
	SkipEvents []string

	// FailSafeReject 风控服务不可达时是否拒绝 (true=fail-closed 生产推荐 / false=fail-open dev)
	FailSafeReject bool
}

// DefaultRiskGateConfig.
func DefaultRiskGateConfig() RiskGateConfig {
	return RiskGateConfig{
		AMLThresholdMinor: 1_000_000,
		SkipEvents:        []string{"hold.expired", "payout.requested"},
		FailSafeReject:    true,
	}
}

// RiskGate 组合 RiskClient + AMLClient + 配置.
type RiskGate struct {
	Cfg  RiskGateConfig
	Risk RiskClient
	AML  AMLClient
	Log  *zap.Logger
}

// Evaluate 决定一笔 plan 能否执行.
//
// 返回:
//   - decision: allow / review / deny
//   - reason:   命中规则 / 拒绝原因 (audit 用)
//   - err:      调风控服务出错 (按 FailSafeReject 决定退化为 deny / allow)
//
// 短路:
//   - tc.Event in SkipEvents → 直接 allow (内部驱动事件)
//   - AmountMinor < AML 阈值 → 跳过 AML, 只跑 Risk
func (g *RiskGate) Evaluate(ctx context.Context, tc TriggerContext, graphKey string) (RiskDecision, string, error) {
	// 内部驱动事件不走风控
	for _, ev := range g.Cfg.SkipEvents {
		if ev == tc.Event {
			return RiskAllow, "skipped (internal event)", nil
		}
	}
	req := RiskRequest{
		Event:       tc.Event,
		ChargeID:    tc.ChargeID,
		MerchantID:  tc.MerchantID,
		AmountMinor: tc.AmountMinor,
		Currency:    tc.Currency,
		Attributes:  tc.Attributes,
		GraphKey:    graphKey,
	}
	// 1) Risk
	if g.Risk == nil {
		return RiskAllow, "no risk client", nil
	}
	riskResult, err := g.Risk.Evaluate(ctx, req)
	if err != nil {
		if g.Log != nil {
			g.Log.Warn("risk evaluate failed", zap.Error(err))
		}
		if g.Cfg.FailSafeReject {
			return RiskDeny, "risk service unreachable; fail-closed", err
		}
		return RiskAllow, "risk service unreachable; fail-open", nil
	}
	if riskResult.Decision == RiskDeny {
		return RiskDeny, "risk: " + riskResult.Reason + " (rule=" + riskResult.RuleID + ")", nil
	}
	if riskResult.Decision == RiskReview {
		return RiskReview, "risk: " + riskResult.Reason + " (rule=" + riskResult.RuleID + ")", nil
	}

	// 2) AML — 超阈才跑
	if tc.AmountMinor < g.Cfg.AMLThresholdMinor {
		return RiskAllow, "below AML threshold", nil
	}
	if g.AML == nil {
		return RiskAllow, "no aml client; allow large amount (verify config)", nil
	}
	amlResult, err := g.AML.Screen(ctx, req)
	if err != nil {
		if g.Log != nil {
			g.Log.Warn("aml screen failed", zap.Error(err))
		}
		if g.Cfg.FailSafeReject {
			return RiskDeny, "aml service unreachable; fail-closed", err
		}
		return RiskAllow, "aml service unreachable; fail-open", nil
	}
	if amlResult.Decision == RiskDeny {
		return RiskDeny, "aml: " + amlResult.Reason + " (rule=" + amlResult.RuleID + ")", nil
	}
	if amlResult.Decision == RiskReview {
		return RiskReview, "aml: " + amlResult.Reason + " (rule=" + amlResult.RuleID + ")", nil
	}
	return RiskAllow, "both risk + aml allow", nil
}

// ErrRiskDenied — engine 据此设 plan.status=rejected.
var ErrRiskDenied = errors.New("risk gate: denied")

// ErrRiskReview — engine 据此设 plan.status=awaiting_review.
var ErrRiskReview = errors.New("risk gate: awaiting review")

// ─── Engine 集成 ───────────────────────────────────────────────────────

// 在 engine.go 的 Engine 结构体 + executeOne 接入 RiskGate:
//
//   type Engine struct {
//       ...
//       RiskGate *RiskGate   // 可选, nil → 不跑风控
//   }
//
// executeOne 流程:
//   1. (现有) Translate → plan
//   2. (NEW) e.RiskGate.Evaluate(plan.tc, graph.key) → allow/review/deny
//        - deny:   plan.Status = "rejected", 发 flow.rejected 事件, 直接 return
//        - review: plan.Status = "awaiting_review", 发 flow.awaiting_review, 暂存; 不执行 (等人工放行)
//        - allow:  继续
//   3. (现有) capability gate / persist / accounting / saga
//
// 见 engine.go runRiskGate() 方法.

// 标准 plan status 常量 (engine 用)
const (
	PlanStatusCreated         = "created"
	PlanStatusExecuting       = "executing"
	PlanStatusCompleted       = "completed"
	PlanStatusFailed          = "failed"
	PlanStatusReversed        = "reversed"
	PlanStatusRejected        = "rejected"         // SP-3B
	PlanStatusAwaitingReview  = "awaiting_review"  // SP-3B
)

// 标准事件 (SP-3B 新增)
const (
	EventFlowRejected       = "flow.rejected"
	EventFlowAwaitingReview = "flow.awaiting_review"
)

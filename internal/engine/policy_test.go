package engine

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// fakeRule 给 policy_test 用的最小 Rule —— 命中固定 Decision + Detail。
type fakeRule struct {
	id, name string
	enabled  bool
	hit      bool
	dec      Decision
	weight   int
}

func (f *fakeRule) ID() string    { return f.id }
func (f *fakeRule) Name() string  { return f.name }
func (f *fakeRule) Type() string  { return "fake" }
func (f *fakeRule) Enabled() bool { return f.enabled }
func (f *fakeRule) Weight() int   { return f.weight }

func (f *fakeRule) Evaluate(_ context.Context, _ *TxnContext) *Hit {
	if !f.hit {
		return nil
	}
	return &Hit{RuleID: f.id, RuleName: f.name, Decision: f.dec, Detail: "hit"}
}

func newEngineWithRules(rules ...Rule) *Engine {
	e := New(zap.NewNop())
	e.rules = rules
	return e
}

func TestPolicyOverride_ThresholdsLowerReviewMin(t *testing.T) {
	// soft-signal rule（hit Decision=Allow，weight 累加 score）→ 测试 score
	// 阈值 mapping 不被 hit-level Worse 兜底干扰。
	e := newEngineWithRules(&fakeRule{id: "r1", enabled: true, hit: true, dec: Allow, weight: 10})
	res := e.Evaluate(context.Background(), &TxnContext{MerchantID: "m1"})
	if res.Decision != Allow {
		t.Fatalf("baseline: expected ALLOW (score=10 < review_min=20), got %s", res.Decision)
	}

	e.SetPolicyStore(NewMemPolicyStore(map[string]*MerchantPolicy{
		"m1": {MerchantID: "m1", ReviewMin: 10},
	}))
	res = e.Evaluate(context.Background(), &TxnContext{MerchantID: "m1"})
	if res.Decision != Review {
		t.Fatalf("override: expected REVIEW (score=10 >= override review_min=10), got %s", res.Decision)
	}

	// 未在 override map 内的商户 → 仍走 baseline。
	res = e.Evaluate(context.Background(), &TxnContext{MerchantID: "m_other"})
	if res.Decision != Allow {
		t.Fatalf("unconfigured merchant should follow baseline, got %s", res.Decision)
	}
}

func TestPolicyOverride_DisabledRule(t *testing.T) {
	e := newEngineWithRules(
		&fakeRule{id: "r_blocked", enabled: true, hit: true, dec: Deny, weight: 50},
		&fakeRule{id: "r_pass", enabled: true, hit: false, dec: Allow},
	)
	// baseline：r_blocked 命中 → DENY
	res := e.Evaluate(context.Background(), &TxnContext{MerchantID: "m1"})
	if res.Decision != Deny {
		t.Fatalf("baseline: expected DENY, got %s", res.Decision)
	}
	// 商户禁用 r_blocked → 不参与评估 → ALLOW
	e.SetPolicyStore(NewMemPolicyStore(map[string]*MerchantPolicy{
		"m1": {MerchantID: "m1", DisabledRules: []string{"r_blocked"}},
	}))
	res = e.Evaluate(context.Background(), &TxnContext{MerchantID: "m1"})
	if res.Decision != Allow {
		t.Fatalf("override: r_blocked disabled, expected ALLOW, got %s (hits=%d)", res.Decision, len(res.Hits))
	}
}

func TestPolicyOverride_WeightOverride(t *testing.T) {
	e := newEngineWithRules(&fakeRule{id: "r1", enabled: true, hit: true, dec: Allow, weight: 5})
	// baseline weight=5 < review_min=20 → ALLOW
	res := e.Evaluate(context.Background(), &TxnContext{MerchantID: "m1"})
	if res.Decision != Allow {
		t.Fatalf("baseline: expected ALLOW, got %s score=%d", res.Decision, res.RiskScore)
	}
	// override weight=25 → 累加后超过 review_min=20 → REVIEW
	e.SetPolicyStore(NewMemPolicyStore(map[string]*MerchantPolicy{
		"m1": {MerchantID: "m1", WeightOverrides: map[string]int{"r1": 25}},
	}))
	res = e.Evaluate(context.Background(), &TxnContext{MerchantID: "m1"})
	if res.RiskScore != 25 || res.Decision != Review {
		t.Fatalf("override: expected score=25 REVIEW, got score=%d %s", res.RiskScore, res.Decision)
	}
}

func TestPolicyOverride_EmptyMerchantIDFallsBackToBaseline(t *testing.T) {
	e := newEngineWithRules(&fakeRule{id: "r1", enabled: true, hit: true, dec: Allow, weight: 25})
	e.SetPolicyStore(NewMemPolicyStore(map[string]*MerchantPolicy{
		"m1": {MerchantID: "m1", DisabledRules: []string{"r1"}},
	}))
	// 无 merchant_id → 不查 override → r1 命中（score=25 >= review_min=20）→ REVIEW
	res := e.Evaluate(context.Background(), &TxnContext{})
	if res.Decision != Review {
		t.Fatalf("empty merchant: expected REVIEW (no override applied), got %s", res.Decision)
	}
}

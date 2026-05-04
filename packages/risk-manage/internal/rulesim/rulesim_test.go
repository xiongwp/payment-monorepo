package rulesim

import (
	"context"
	"testing"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

// amountGTRule 命中所有 amount > threshold 的样本。
type amountGTRule struct {
	id        string
	threshold int64
}

func (r amountGTRule) ID() string                    { return r.id }
func (r amountGTRule) Name() string                  { return r.id }
func (r amountGTRule) Type() string                  { return "amount_gt" }
func (r amountGTRule) Enabled() bool                 { return true }
func (r amountGTRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn != nil && txn.Amount > r.threshold {
		return &engine.Hit{RuleID: r.id, RuleName: r.id, Decision: engine.Deny}
	}
	return nil
}

// nilRule 永远 miss。
type nilRule struct{ id string }

func (r nilRule) ID() string                                            { return r.id }
func (r nilRule) Name() string                                          { return r.id }
func (r nilRule) Type() string                                          { return "noop" }
func (r nilRule) Enabled() bool                                         { return true }
func (r nilRule) Evaluate(_ context.Context, _ *engine.TxnContext) *engine.Hit { return nil }

func mkAudit(id string, amount int64, verdict string) *audit.DecisionAudit {
	return &audit.DecisionAudit{
		DecisionID: id,
		OccurredAt: time.Now(),
		Verdict:    verdict,
		Input: audit.AuditInput{
			Amount:   amount,
			Currency: "USD",
		},
	}
}

func TestSimulate_HitsAndWouldNewlyBlock(t *testing.T) {
	// 候选规则：amount > 100 → DENY
	// 5 笔决策：3 笔 amount=200 currently ALLOW + 2 笔 amount=50 ALLOW
	// 期望：3 hits（全是 newly_block，因为当前 verdict=ALLOW）
	audits := []*audit.DecisionAudit{
		mkAudit("d1", 200, "ALLOW"),
		mkAudit("d2", 200, "ALLOW"),
		mkAudit("d3", 200, "ALLOW"),
		mkAudit("d4", 50, "ALLOW"),
		mkAudit("d5", 50, "ALLOW"),
	}
	r := amountGTRule{id: "candidate", threshold: 100}
	res := Simulate(context.Background(), r, audits, nil)
	if res.Sample != 5 {
		t.Fatalf("sample=%d", res.Sample)
	}
	if res.Hits != 3 {
		t.Fatalf("hits=%d, want 3", res.Hits)
	}
	if res.WouldNewlyBlock != 3 || res.WouldKeepBlock != 0 {
		t.Fatalf("wanted 3 newly + 0 keep; got %d / %d", res.WouldNewlyBlock, res.WouldKeepBlock)
	}
	if res.HitRate < 0.59 || res.HitRate > 0.61 {
		t.Fatalf("hit_rate ~0.6; got %.3f", res.HitRate)
	}
}

func TestSimulate_KeepBlockWhenAlreadyDeny(t *testing.T) {
	audits := []*audit.DecisionAudit{
		mkAudit("d1", 200, "DENY"),
		mkAudit("d2", 200, "REVIEW"),
		mkAudit("d3", 200, "ALLOW"),
	}
	r := amountGTRule{id: "candidate", threshold: 100}
	res := Simulate(context.Background(), r, audits, nil)
	if res.WouldKeepBlock != 2 {
		t.Fatalf("wanted 2 keep; got %d", res.WouldKeepBlock)
	}
	if res.WouldNewlyBlock != 1 {
		t.Fatalf("wanted 1 newly; got %d", res.WouldNewlyBlock)
	}
}

func TestSimulate_PrecisionEstimateNeeds5Outcomes(t *testing.T) {
	rec := feedback.NewMemRecorder(0)
	// 6 笔候选 hit，5 个有 outcome：4 fraud / 1 legit
	audits := []*audit.DecisionAudit{}
	for i := 0; i < 6; i++ {
		id := "p" + string(rune('0'+i))
		audits = append(audits, mkAudit(id, 200, "ALLOW"))
		if i < 5 {
			_ = rec.Record(feedback.Outcome{
				DecisionID: id, Source: feedback.SourceDispute, IsFraud: i < 4,
			})
		}
	}
	r := amountGTRule{id: "c", threshold: 100}
	res := Simulate(context.Background(), r, audits, rec)
	if res.HitsWithOutcome != 5 || res.HitsTrueFraud != 4 {
		t.Fatalf("expected 5 outcome / 4 fraud; got %+v", res)
	}
	if res.EstimatedPrecision < 0.79 || res.EstimatedPrecision > 0.81 {
		t.Fatalf("expected precision ~0.8; got %.3f", res.EstimatedPrecision)
	}
}

func TestSimulate_LessThan5OutcomesAddsNote(t *testing.T) {
	rec := feedback.NewMemRecorder(0)
	audits := []*audit.DecisionAudit{
		mkAudit("d1", 200, "ALLOW"),
		mkAudit("d2", 200, "ALLOW"),
	}
	_ = rec.Record(feedback.Outcome{DecisionID: "d1", Source: feedback.SourceDispute, IsFraud: true})
	r := amountGTRule{id: "c", threshold: 100}
	res := Simulate(context.Background(), r, audits, rec)
	if res.EstimatedPrecision != 0 {
		t.Fatalf("low-N should not estimate precision; got %.3f", res.EstimatedPrecision)
	}
	if len(res.Notes) == 0 {
		t.Fatal("expected a note about low outcome count")
	}
}

func TestSimulate_ZeroHitsAddsNote(t *testing.T) {
	audits := []*audit.DecisionAudit{
		mkAudit("d1", 50, "ALLOW"),
		mkAudit("d2", 50, "ALLOW"),
	}
	r := nilRule{id: "noop"}
	res := Simulate(context.Background(), r, audits, nil)
	if res.Hits != 0 {
		t.Fatalf("expected 0 hits; got %d", res.Hits)
	}
	if len(res.Notes) == 0 {
		t.Fatal("expected note suggesting shadow mode")
	}
}

func TestSimulate_NilRuleEmptyAudits(t *testing.T) {
	res := Simulate(context.Background(), nil, nil, nil)
	if res.Sample != 0 || res.Hits != 0 {
		t.Fatalf("nil/empty should return zero result; got %+v", res)
	}
}

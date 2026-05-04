package synthetic

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/risk-manage/internal/engine"
)

type stubScreener struct {
	calls atomic.Int64
	verdict engine.Decision
	hits   []engine.Hit
}

func (s *stubScreener) Screen(_ context.Context, _ *engine.TxnContext) *engine.Result {
	s.calls.Add(1)
	return &engine.Result{Decision: s.verdict, Hits: s.hits}
}

func TestRun_OK_WhenVerdictMatches(t *testing.T) {
	s := &stubScreener{verdict: engine.Allow}
	w := NewWorker(s, []Probe{{
		Name:           "p1",
		Build:          func() *engine.TxnContext { return &engine.TxnContext{} },
		ExpectedVerdict: engine.Allow,
	}}, time.Second, zap.NewNop())
	w.runAll(context.Background())
	if s.calls.Load() != 1 {
		t.Fatal("screener should be called once")
	}
}

func TestRun_Mismatch_LogsAndCounts(t *testing.T) {
	s := &stubScreener{verdict: engine.Allow}
	// 期望 Deny，实际 Allow → mismatch
	w := NewWorker(s, []Probe{{
		Name:           "p_mismatch",
		Build:          func() *engine.TxnContext { return &engine.TxnContext{} },
		ExpectedVerdict: engine.Deny,
	}}, time.Second, zap.NewNop())
	w.runAll(context.Background())
	// 没法 introspect prometheus counter 直接确认；至少不 panic + 调用计数对
	if s.calls.Load() != 1 {
		t.Fatal("should still call screener once")
	}
}

func TestRun_ExpectedRuleHits_Subset(t *testing.T) {
	// hits 包含 expected → ok
	s := &stubScreener{verdict: engine.Deny, hits: []engine.Hit{
		{RuleID: "rule_a"}, {RuleID: "rule_b"},
	}}
	w := NewWorker(s, []Probe{{
		Name:             "expect_a",
		Build:            func() *engine.TxnContext { return &engine.TxnContext{} },
		ExpectedVerdict:  engine.Deny,
		ExpectedRuleHits: []string{"rule_a"},
	}}, time.Second, zap.NewNop())
	w.runAll(context.Background())

	// 缺 expected → 视作 mismatch
	s2 := &stubScreener{verdict: engine.Deny, hits: []engine.Hit{{RuleID: "rule_b"}}}
	w2 := NewWorker(s2, []Probe{{
		Name:             "expect_a_missing",
		Build:            func() *engine.TxnContext { return &engine.TxnContext{} },
		ExpectedVerdict:  engine.Deny,
		ExpectedRuleHits: []string{"rule_a"},
	}}, time.Second, zap.NewNop())
	w2.runAll(context.Background())
}

func TestRun_NilBuild_Errors(t *testing.T) {
	s := &stubScreener{verdict: engine.Allow}
	w := NewWorker(s, []Probe{{Name: "bad"}}, time.Second, zap.NewNop())
	w.runAll(context.Background())
	// screener should NOT be called with bad probe
	if s.calls.Load() != 0 {
		t.Fatal("nil Build should skip screen call")
	}
}

func TestStart_RespectsContextCancel(t *testing.T) {
	s := &stubScreener{verdict: engine.Allow}
	w := NewWorker(s, DefaultProbes(), 10*time.Millisecond, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Start(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not exit on ctx cancel")
	}
}

func TestDefaultProbes_HasExpectedSet(t *testing.T) {
	probes := DefaultProbes()
	if len(probes) < 3 {
		t.Fatalf("expected ≥3 default probes, got %d", len(probes))
	}
	names := map[string]bool{}
	for _, p := range probes {
		names[p.Name] = true
		if p.Build == nil {
			t.Errorf("probe %s missing Build", p.Name)
		}
	}
	for _, want := range []string{"clean_signup_allow", "disposable_email_register_deny"} {
		if !names[want] {
			t.Errorf("missing probe %s", want)
		}
	}
}

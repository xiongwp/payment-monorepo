package matcher

import (
	"context"
	"testing"
	"time"

	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/store"
)

func mkEvt(svc, table, pk string, after map[string]any) *store.Event {
	return &store.Event{
		Service: svc, Table: table, PK: pk,
		Op: "INSERT", After: after,
	}
}

func mkEvtIdx(svc, table, pk string, after map[string]any, idx map[string]string) *store.Event {
	e := mkEvt(svc, table, pk, after)
	e.Indexes = idx
	return e
}

func TestPresenceRule_AllPresent_Matched(t *testing.T) {
	r := &CrossServicePresenceRule{
		RuleName: "pi_full", BizKey: "pi_id",
		ExpectedServices: []string{"order-core", "payment-channel", "accounting-system"},
	}
	events := []*store.Event{
		mkEvt("order-core", "pi", "pi_1", nil),
		mkEvt("payment-channel", "tx", "tx_1", nil),
		mkEvt("accounting-system", "le", "le_1", nil),
	}
	res, err := r.Match(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"}, events)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != VerdictMatched {
		t.Errorf("want matched, got %s", res.Verdict)
	}
}

func TestPresenceRule_OneMissing_Pending(t *testing.T) {
	r := &CrossServicePresenceRule{
		RuleName: "pi_full", BizKey: "pi_id",
		ExpectedServices: []string{"order-core", "payment-channel", "accounting-system"},
	}
	events := []*store.Event{
		mkEvt("order-core", "pi", "pi_1", nil),
		mkEvt("payment-channel", "tx", "tx_1", nil),
	}
	res, _ := r.Match(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"}, events)
	if res.Verdict != VerdictPending {
		t.Errorf("want pending, got %s", res.Verdict)
	}
}

func TestPresenceRule_Empty_Orphan(t *testing.T) {
	r := &CrossServicePresenceRule{
		RuleName: "pi_full", BizKey: "pi_id",
		ExpectedServices: []string{"order-core", "payment-channel"},
	}
	res, _ := r.Match(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"}, nil)
	if res.Verdict != VerdictOrphan {
		t.Errorf("want orphan, got %s", res.Verdict)
	}
}

func TestAmountEquality_AllEqual_Matched(t *testing.T) {
	r := &AmountEqualityRule{
		RuleName: "amt", BizKey: "pi_id",
		Pairs: []AmountPair{
			{"order-core", "pi"},
			{"payment-channel", "tx"},
		},
	}
	events := []*store.Event{
		mkEvt("order-core", "pi", "pi_1", map[string]any{"amount": float64(1000)}),
		mkEvt("payment-channel", "tx", "tx_1", map[string]any{"amount": int64(1000)}),
	}
	res, _ := r.Match(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"}, events)
	if res.Verdict != VerdictMatched {
		t.Errorf("want matched, got %s detail=%+v", res.Verdict, res.Detail)
	}
}

func TestAmountEquality_Mismatch_Mismatched(t *testing.T) {
	r := &AmountEqualityRule{
		RuleName: "amt", BizKey: "pi_id",
		Pairs: []AmountPair{
			{"order-core", "pi"}, {"payment-channel", "tx"},
		},
	}
	events := []*store.Event{
		mkEvt("order-core", "pi", "pi_1", map[string]any{"amount": float64(1000)}),
		mkEvt("payment-channel", "tx", "tx_1", map[string]any{"amount": float64(900)}),
	}
	res, _ := r.Match(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"}, events)
	if res.Verdict != VerdictMismatched {
		t.Errorf("want mismatched, got %s", res.Verdict)
	}
}

func TestAmountEquality_NotAllPresent_Pending(t *testing.T) {
	r := &AmountEqualityRule{
		RuleName: "amt", BizKey: "pi_id",
		Pairs: []AmountPair{
			{"order-core", "pi"}, {"payment-channel", "tx"},
		},
	}
	events := []*store.Event{
		mkEvt("order-core", "pi", "pi_1", map[string]any{"amount": 1000}),
	}
	res, _ := r.Match(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"}, events)
	if res.Verdict != VerdictPending {
		t.Errorf("want pending, got %s", res.Verdict)
	}
}

func TestRegistry_DuplicateRegister(t *testing.T) {
	reg := NewRegistry()
	r := &CrossServicePresenceRule{RuleName: "x"}
	if err := reg.Register(r); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(r); err == nil {
		t.Error("expect error on duplicate")
	}
}

// panicRule 测试 safeMatch.
type panicRule struct{}

func (panicRule) Name() string { return "panic-rule" }
func (panicRule) Match(_ context.Context, _ candidate.TriggerKey, _ []*store.Event) (MatchResult, error) {
	panic("intentional")
}

func TestRegistry_EvalAll_PanicSafe(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(&CrossServicePresenceRule{RuleName: "ok", BizKey: "pi_id",
		ExpectedServices: []string{"order-core"}})
	_ = reg.Register(panicRule{})
	res := reg.EvalAll(context.Background(),
		candidate.TriggerKey{BizKey: "pi_id", Value: "pi_x"},
		[]*store.Event{mkEvt("order-core", "pi", "pi_x", nil)})
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	gotError := false
	for _, r := range res {
		if r.RuleName == "panic-rule" && r.Verdict == VerdictError {
			gotError = true
		}
	}
	if !gotError {
		t.Error("panic should produce VerdictError")
	}
}

// 端到端: worker pop -> match -> publish.
type capturePublisher struct {
	results []MatchResult
}

func (c *capturePublisher) Publish(_ context.Context, r MatchResult) error {
	c.results = append(c.results, r)
	return nil
}

func TestWorker_EndToEnd(t *testing.T) {
	layer := candidate.NewMemoryLayer(candidate.Config{DefaultTriggerThreshold: 2})
	_, _ = layer.Put(context.Background(),
		mkEvtIdx("order-core", "pi", "pi_1",
			map[string]any{"amount": 1000},
			map[string]string{"pi_id": "pi_1"}))
	_, _ = layer.Put(context.Background(),
		mkEvtIdx("payment-channel", "tx", "tx_1",
			map[string]any{"amount": 1000},
			map[string]string{"pi_id": "pi_1"}))

	reg := NewRegistry()
	_ = reg.Register(&CrossServicePresenceRule{
		RuleName: "presence", BizKey: "pi_id",
		ExpectedServices: []string{"order-core", "payment-channel"},
	})

	cap := &capturePublisher{}
	w := NewWorker(WorkerConfig{
		WorkerID: "test", BatchSize: 10, PollTimeout: 100 * time.Millisecond,
	}, layer, reg, cap, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	doneCh := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(doneCh)
	}()

	// 等结果或超时
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) && len(cap.results) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-doneCh
	if len(cap.results) == 0 {
		t.Fatal("no results captured")
	}
	if cap.results[0].Verdict != VerdictMatched {
		t.Errorf("expected matched, got %s", cap.results[0].Verdict)
	}
}

func TestToInt64_Variants(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{int(42), 42, true},
		{int32(42), 42, true},
		{int64(42), 42, true},
		{float64(42), 42, true},
		{float32(42), 42, true},
		{"42", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt64(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("toInt64(%v)=(%d,%v) want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

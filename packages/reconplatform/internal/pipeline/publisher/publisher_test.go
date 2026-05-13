package publisher

import (
	"context"
	"errors"
	"testing"

	"reconcile-system/internal/pipeline/candidate"
	"reconcile-system/internal/pipeline/matcher"
)

func TestNoopPublisher_Counts(t *testing.T) {
	n := &NoopPublisher{}
	r := matcher.MatchResult{
		TriggerKey: candidate.TriggerKey{BizKey: "pi_id", Value: "pi_1"},
		Verdict:    matcher.VerdictMatched,
	}
	for i := 0; i < 5; i++ {
		_ = n.Publish(context.Background(), r)
	}
	if n.Count.Load() != 5 {
		t.Errorf("count=%d", n.Count.Load())
	}
}

// recordingPub 收集 publish 调用.
type recordingPub struct {
	calls  int
	last   matcher.MatchResult
	failOn int
}

func (r *recordingPub) Publish(_ context.Context, m matcher.MatchResult) error {
	r.calls++
	r.last = m
	if r.failOn == r.calls {
		return errors.New("forced fail")
	}
	return nil
}

func TestMultiSink_AllSinksReached(t *testing.T) {
	a := &recordingPub{}
	b := &recordingPub{}
	c := &recordingPub{}
	ms := &MultiSink{Sinks: []matcher.Publisher{a, b, c}}
	r := matcher.MatchResult{Verdict: matcher.VerdictMatched, RuleName: "x"}
	if err := ms.Publish(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 || b.calls != 1 || c.calls != 1 {
		t.Errorf("all sinks should be called once: %d/%d/%d", a.calls, b.calls, c.calls)
	}
}

func TestMultiSink_FirstErrorReturnedButOthersStillCalled(t *testing.T) {
	a := &recordingPub{failOn: 1}
	b := &recordingPub{}
	ms := &MultiSink{Sinks: []matcher.Publisher{a, b}}
	err := ms.Publish(context.Background(), matcher.MatchResult{})
	if err == nil {
		t.Fatal("expect error")
	}
	if b.calls != 1 {
		t.Error("b should still be called after a fails")
	}
}

func TestDefaultConfig_Sensible(t *testing.T) {
	c := DefaultConfig([]string{"kafka:9092"})
	if c.Topic != "recon.results" {
		t.Errorf("topic default: %s", c.Topic)
	}
	if c.BatchTimeout == 0 {
		t.Error("batch timeout not set")
	}
}

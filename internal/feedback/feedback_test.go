package feedback

import (
	"testing"
	"time"
)

func TestMemRecorder_BasicRecordAndGet(t *testing.T) {
	r := NewMemRecorder(0)
	o := Outcome{DecisionID: "abc", Source: SourceReviewHuman, IsFraud: true, Actor: "ops"}
	if err := r.Record(o); err != nil {
		t.Fatal(err)
	}
	got := r.Get("abc")
	if len(got) != 1 || got[0].DecisionID != "abc" || !got[0].IsFraud {
		t.Fatalf("get failed: %+v", got)
	}
	if got[0].At.IsZero() {
		t.Fatal("At should be auto-filled")
	}
}

func TestMemRecorder_MultipleSourcesPerDecision(t *testing.T) {
	r := NewMemRecorder(0)
	r.Record(Outcome{DecisionID: "x", Source: SourceReviewHuman, IsFraud: false})
	r.Record(Outcome{DecisionID: "x", Source: SourceDispute, IsFraud: true})
	got := r.Get("x")
	if len(got) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(got))
	}
}

func TestMemRecorder_RecentDescOrder(t *testing.T) {
	r := NewMemRecorder(0)
	r.Record(Outcome{DecisionID: "a", Source: SourceReviewHuman, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	r.Record(Outcome{DecisionID: "b", Source: SourceDispute, At: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)})
	r.Record(Outcome{DecisionID: "c", Source: SourceMerchantConfirm, At: time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)})
	out := r.Recent(10)
	if len(out) != 3 {
		t.Fatalf("expected 3, got %d", len(out))
	}
	if out[0].DecisionID != "b" || out[1].DecisionID != "c" || out[2].DecisionID != "a" {
		t.Fatalf("DESC by At failed: %+v", out)
	}
}

func TestMemRecorder_RecordRequiresDecisionID(t *testing.T) {
	r := NewMemRecorder(0)
	if err := r.Record(Outcome{Source: SourceReviewHuman}); err == nil {
		t.Fatal("expected error on empty decision_id")
	}
}

func TestMemRecorder_RecordRequiresSource(t *testing.T) {
	r := NewMemRecorder(0)
	if err := r.Record(Outcome{DecisionID: "x"}); err == nil {
		t.Fatal("expected error on empty source")
	}
}

func TestMemRecorder_AllCapBounds(t *testing.T) {
	r := NewMemRecorder(3)
	for i := 0; i < 10; i++ {
		r.Record(Outcome{DecisionID: "id" + string(rune('a'+i)), Source: SourceReviewHuman})
	}
	out := r.Recent(100)
	if len(out) > 3 {
		t.Fatalf("Recent should cap at maxAll=3, got %d", len(out))
	}
}

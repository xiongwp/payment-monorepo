package feedback

import (
	"testing"

	"github.com/xiongwp/risk-manage/internal/audit"
)

// TestChainRecorder_RecordWritesToChain：Outcome.Record 应同时落 inner +
// 上链；verify 应通过。
func TestChainRecorder_RecordWritesToChain(t *testing.T) {
	mem := audit.NewMemStreamSink(16)
	cw := audit.NewChainWriter("outcomes", mem)
	base := NewMemRecorder(0)
	rec := WrapWithChain(base, cw)

	for _, o := range []Outcome{
		{DecisionID: "d1", Source: SourceReviewHuman, IsFraud: false, Actor: "alice"},
		{DecisionID: "d2", Source: SourceDispute, IsFraud: true, Actor: "system"},
	} {
		if err := rec.Record(o); err != nil {
			t.Fatal(err)
		}
	}

	// inner 仍然能查到（chain wrapper 不破坏原行为）
	if got := base.Get("d1"); len(got) != 1 {
		t.Fatalf("inner Get(d1): expected 1, got %d", len(got))
	}
	// chain：2 条 record，verify 应通过
	recent := mem.Recent(10)
	if len(recent) != 2 {
		t.Fatalf("expected 2 chain records, got %d", len(recent))
	}
	records := make([]*audit.StreamRecord, len(recent))
	for i, r := range recent {
		records[len(recent)-1-i] = r
	}
	if idx, err := audit.VerifyStreamChain(records, ""); err != nil {
		t.Fatalf("chain verify failed at %d: %v", idx, err)
	}
}

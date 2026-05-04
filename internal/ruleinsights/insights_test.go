package ruleinsights

import (
	"context"
	"testing"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

func mkA(t *testing.T, id string, when time.Time, hitRules ...string) *audit.DecisionAudit {
	t.Helper()
	hits := make([]audit.AuditHit, 0, len(hitRules))
	for _, r := range hitRules {
		hits = append(hits, audit.AuditHit{RuleID: r})
	}
	return &audit.DecisionAudit{
		DecisionID: id,
		OccurredAt: when,
		Verdict:    "DENY",
		Hits:       hits,
	}
}

func TestCompute_SilentRulesFlagged(t *testing.T) {
	now := time.Now()
	audits := []*audit.DecisionAudit{
		mkA(t, "d1", now.Add(-1*time.Hour), "r_active"),
		mkA(t, "d2", now.Add(-10*24*time.Hour), "r_old"),
	}
	got := Compute(audits, nil, []string{"r_active", "r_old", "r_never"}, 7*24*time.Hour)

	bm := map[string]RuleStats{}
	for _, s := range got {
		bm[s.RuleID] = s
	}
	if bm["r_active"].IsSilent {
		t.Fatal("r_active should not be silent (1h ago)")
	}
	if !bm["r_old"].IsSilent {
		t.Fatal("r_old should be silent (10d ago)")
	}
	if !bm["r_never"].IsSilent {
		t.Fatal("r_never never hit; should be silent")
	}
	if bm["r_never"].DaysSinceHit != -1 {
		t.Fatalf("never-hit rule should have DaysSinceHit=-1; got %v", bm["r_never"].DaysSinceHit)
	}
}

func TestCompute_PrecisionAndROI(t *testing.T) {
	now := time.Now()
	rec := feedback.NewMemRecorder(0)
	// r_good 命中 6 次，5 个有 outcome 4 个 fraud → precision=0.8 ROI=6*0.8=4.8
	for i := 0; i < 6; i++ {
		id := "g" + string(rune('0'+i))
		_ = id
	}
	audits := []*audit.DecisionAudit{
		mkA(t, "g0", now, "r_good"),
		mkA(t, "g1", now, "r_good"),
		mkA(t, "g2", now, "r_good"),
		mkA(t, "g3", now, "r_good"),
		mkA(t, "g4", now, "r_good"),
		mkA(t, "g5", now, "r_good"), // 第 6 个不打 outcome
	}
	for i, did := range []string{"g0", "g1", "g2", "g3", "g4"} {
		_ = rec.Record(feedback.Outcome{
			DecisionID: did, Source: feedback.SourceDispute, IsFraud: i < 4,
		})
	}
	got := Compute(audits, rec, nil, 7*24*time.Hour)
	if len(got) != 1 || got[0].RuleID != "r_good" {
		t.Fatalf("expected single rule r_good; got %+v", got)
	}
	s := got[0]
	if s.HitsInWindow != 6 {
		t.Fatalf("expected 6 hits; got %d", s.HitsInWindow)
	}
	if s.LabelCount != 5 || s.FraudCount != 4 {
		t.Fatalf("expected label=5 fraud=4; got label=%d fraud=%d", s.LabelCount, s.FraudCount)
	}
	if s.Precision < 0.79 || s.Precision > 0.81 {
		t.Fatalf("expected precision~0.8; got %.3f", s.Precision)
	}
	if s.ROI < 4.7 || s.ROI > 4.9 {
		t.Fatalf("expected ROI~4.8; got %.3f", s.ROI)
	}
}

func TestCompute_PrecisionRequiresMin5Labels(t *testing.T) {
	now := time.Now()
	rec := feedback.NewMemRecorder(0)
	// 只有 4 个 outcome → 不计算 precision（避免噪音）
	audits := []*audit.DecisionAudit{
		mkA(t, "x0", now, "r_low_n"),
		mkA(t, "x1", now, "r_low_n"),
		mkA(t, "x2", now, "r_low_n"),
		mkA(t, "x3", now, "r_low_n"),
	}
	for _, did := range []string{"x0", "x1", "x2", "x3"} {
		_ = rec.Record(feedback.Outcome{DecisionID: did, Source: feedback.SourceDispute, IsFraud: true})
	}
	got := Compute(audits, rec, nil, 7*24*time.Hour)
	if got[0].Precision != 0 || got[0].ROI != 0 {
		t.Fatalf("low-N rules should have precision=0 ROI=0; got %+v", got[0])
	}
}

// 输出按 ROI 降序：高 ROI 在前，零 ROI 按 hits 兜底
func TestCompute_SortedByROI(t *testing.T) {
	now := time.Now()
	rec := feedback.NewMemRecorder(0)
	audits := []*audit.DecisionAudit{}
	// r_low_roi: 6 hits, 5 labels, 1 fraud → P=0.2 ROI=1.2
	for i := 0; i < 6; i++ {
		did := "lo" + string(rune('0'+i))
		audits = append(audits, mkA(t, did, now, "r_low_roi"))
		if i < 5 {
			_ = rec.Record(feedback.Outcome{DecisionID: did, Source: feedback.SourceDispute, IsFraud: i == 0})
		}
	}
	// r_high_roi: 3 hits, 5 labels (impossible? we share label), 4 fraud → P=0.8 ROI=2.4
	// just 5 hits, all 5 with fraud=true label
	for i := 0; i < 5; i++ {
		did := "hi" + string(rune('0'+i))
		audits = append(audits, mkA(t, did, now, "r_high_roi"))
		_ = rec.Record(feedback.Outcome{DecisionID: did, Source: feedback.SourceDispute, IsFraud: true})
	}
	got := Compute(audits, rec, nil, 7*24*time.Hour)
	if len(got) != 2 {
		t.Fatalf("expected 2 rules; got %d", len(got))
	}
	if got[0].RuleID != "r_high_roi" {
		t.Fatalf("expected r_high_roi first (highest ROI); got %s (ROI=%.2f) first",
			got[0].RuleID, got[0].ROI)
	}
}

func TestCompute_NilAuditsAndRecorder(t *testing.T) {
	// 边界：nil audit 列表 + 全部 nil 不该 panic
	got := Compute(nil, nil, []string{"r1"}, time.Hour)
	if len(got) != 1 || !got[0].IsSilent {
		t.Fatalf("expected single silent rule; got %+v", got)
	}
}

// MemSink 集成 smoke test：从 sink 读 audit，传给 Compute
func TestCompute_FromMemSink(t *testing.T) {
	sink := audit.NewMemSink(8)
	now := time.Now()
	sink.Write(context.Background(), mkA(t, "d1", now, "r1"))
	sink.Write(context.Background(), mkA(t, "d2", now, "r2"))
	got := Compute(sink.Recent(0), nil, nil, time.Hour)
	if len(got) != 2 {
		t.Fatalf("expected 2 rules; got %d", len(got))
	}
}

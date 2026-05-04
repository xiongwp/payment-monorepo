package featurestore

import (
	"context"
	"testing"
	"time"

	"github.com/xiongwp/risk-manage/internal/mlscore"
)

func snapWithLabel(id, verdict string, isFraud bool, ageHours int) Snapshot {
	b := isFraud
	now := time.Now().Add(-time.Duration(ageHours) * time.Hour)
	return Snapshot{
		DecisionID:   id,
		OccurAt:      now,
		Features:     &mlscore.Features{CustomerID: id, Currency: "PHP"},
		Verdict:      verdict,
		OutcomeLabel: &b,
	}
}

func TestComputeRecall_PerfectClassifier(t *testing.T) {
	s := NewMemStore(0)
	ctx := context.Background()
	// 真 fraud 都 DENY，真 legit 都 ALLOW
	s.Save(ctx, snapWithLabel("a", "DENY", true, 1))
	s.Save(ctx, snapWithLabel("b", "DENY", true, 2))
	s.Save(ctx, snapWithLabel("c", "ALLOW", false, 3))
	s.Save(ctx, snapWithLabel("d", "ALLOW", false, 4))
	r := s.ComputeRecall(7)
	if r.PrecisionAtBlock != 1.0 || r.RecallAtBlock != 1.0 {
		t.Fatalf("perfect classifier should give P=1 R=1; got P=%v R=%v",
			r.PrecisionAtBlock, r.RecallAtBlock)
	}
	if r.F1Score != 1.0 {
		t.Errorf("F1 should be 1.0, got %v", r.F1Score)
	}
}

func TestComputeRecall_AllAllow_ZeroRecall(t *testing.T) {
	// 全放行 → recall=0（漏抓所有 fraud），precision=NaN（除0）变 0
	s := NewMemStore(0)
	ctx := context.Background()
	s.Save(ctx, snapWithLabel("a", "ALLOW", true, 1))
	s.Save(ctx, snapWithLabel("b", "ALLOW", true, 2))
	s.Save(ctx, snapWithLabel("c", "ALLOW", false, 3))
	r := s.ComputeRecall(7)
	if r.RecallAtBlock != 0 {
		t.Errorf("all-allow recall should be 0, got %v", r.RecallAtBlock)
	}
	if r.FalsePositiveRate != 0 {
		t.Errorf("all-allow FPR should be 0, got %v", r.FalsePositiveRate)
	}
}

func TestComputeRecall_AllDeny_HighFPR(t *testing.T) {
	// 全拦 → recall=1 但误伤巨多 → FPR=1
	s := NewMemStore(0)
	ctx := context.Background()
	s.Save(ctx, snapWithLabel("a", "DENY", true, 1))
	s.Save(ctx, snapWithLabel("b", "DENY", false, 2)) // 误杀
	s.Save(ctx, snapWithLabel("c", "DENY", false, 3)) // 误杀
	r := s.ComputeRecall(7)
	if r.RecallAtBlock != 1.0 {
		t.Errorf("all-deny recall should be 1, got %v", r.RecallAtBlock)
	}
	if r.FalsePositiveRate != 1.0 {
		t.Errorf("all-deny FPR should be 1, got %v", r.FalsePositiveRate)
	}
	// precision = 1/3
	expected := 1.0 / 3.0
	if r.PrecisionAtBlock < expected-0.01 || r.PrecisionAtBlock > expected+0.01 {
		t.Errorf("precision should be ~0.333, got %v", r.PrecisionAtBlock)
	}
}

func TestComputeRecall_OutsideWindowExcluded(t *testing.T) {
	s := NewMemStore(0)
	ctx := context.Background()
	// 30 天前的样本 — windowDays=7 应排除
	s.Save(ctx, snapWithLabel("old", "DENY", true, 30*24))
	s.Save(ctx, snapWithLabel("new", "ALLOW", false, 1))
	r := s.ComputeRecall(7)
	if r.LabeledSamples != 1 {
		t.Errorf("only new sample should count, got %d", r.LabeledSamples)
	}
}

func TestComputeRecall_NoLabeledSamplesEmpty(t *testing.T) {
	s := NewMemStore(0)
	r := s.ComputeRecall(7)
	if r.LabeledSamples != 0 || r.PrecisionAtBlock != 0 {
		t.Errorf("empty store should give zeros, got %+v", r)
	}
}

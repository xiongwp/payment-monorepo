package mlscore

import (
	"math/rand"
	"testing"
)

func TestABTracker_RecordAndSnapshotOrder(t *testing.T) {
	tr := NewABTracker(8)
	tr.Record("d1", 0.1, []NamedResult{{Name: "v2", Score: 0.2}})
	tr.Record("d2", 0.5, []NamedResult{{Name: "v2", Score: 0.6}})
	got := tr.Snapshot()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries; got %d", len(got))
	}
	// 最新在前（跟 MemSink Recent 一致约定）
	if got[0].DecisionID != "d2" || got[1].DecisionID != "d1" {
		t.Fatalf("expected [d2, d1]; got %+v", got)
	}
}

func TestABTracker_RingOverwriteCleansIndex(t *testing.T) {
	tr := NewABTracker(2)
	tr.Record("d1", 0.1, nil)
	tr.Record("d2", 0.2, nil)
	tr.Record("d3", 0.3, nil) // 覆盖 d1
	if _, ok := tr.byID["d1"]; ok {
		t.Fatal("d1 should have been evicted from index")
	}
	if _, ok := tr.byID["d3"]; !ok {
		t.Fatal("d3 should be in index")
	}
}

func TestABTracker_RecordSkipsEmptyDecisionID(t *testing.T) {
	tr := NewABTracker(4)
	tr.Record("", 0.5, []NamedResult{{Name: "v2", Score: 0.6}})
	if got := tr.Snapshot(); len(got) != 0 {
		t.Fatal("empty decision_id should be skipped")
	}
}

func TestABTracker_ChallengerErrorOmitted(t *testing.T) {
	tr := NewABTracker(4)
	tr.Record("d1", 0.5, []NamedResult{
		{Name: "v2", Score: 0.7},
		{Name: "v3", Error: "timeout"}, // 不该写进去
	})
	got := tr.Snapshot()
	if len(got[0].ChallengerScores) != 1 || got[0].ChallengerScores["v2"] != 0.7 {
		t.Fatalf("expected only v2; got %+v", got[0].ChallengerScores)
	}
}

// 整合测试：synthetic 数据，challenger 比 champion 好 0.1，bootstrap 应给出
// CI 全 > 0 → recommend promote。
func TestReport_PromotesObviouslyBetterChallenger(t *testing.T) {
	tr := NewABTracker(2000)
	rng := rand.New(rand.NewSource(7))
	outcomes := map[string]bool{}
	for i := 0; i < 400; i++ {
		isFraud := rng.Float64() < 0.3
		// 噪声范围跟信号叠加，让两个分类器都不是 perfect。
		// champion: 信号 0.3 → 60-70% AUC
		champ := 0.3*boolToF(isFraud) + rng.Float64()
		// challenger: 信号 0.6 → 75-85% AUC（明显更好）
		chal := 0.6*boolToF(isFraud) + rng.Float64()
		id := "d" + string(rune('a'+i%26)) + string(rune('0'+i/26)) + string(rune('A'+i/(26*8)))
		tr.Record(id, champ, []NamedResult{{Name: "v2", Score: chal}})
		outcomes[id] = isFraud
	}
	getOutcome := func(id string) (bool, bool) {
		v, ok := outcomes[id]
		return v, ok
	}
	rep := tr.Report(getOutcome, 30, 200)
	if len(rep) != 1 {
		t.Fatalf("expected 1 challenger; got %d", len(rep))
	}
	r := rep[0]
	if r.LabeledSamples < 100 {
		t.Fatalf("expected >= 100 labeled; got %d", r.LabeledSamples)
	}
	if r.AUCDiff <= 0 {
		t.Fatalf("expected challenger AUC > champion AUC; got diff=%.4f", r.AUCDiff)
	}
	// 显著 → CI 低端 > 0
	if r.CILow <= 0 {
		t.Fatalf("CI low should be > 0 for clearly better challenger; got [%f, %f]", r.CILow, r.CIHigh)
	}
	if r.Recommendation != "promote" {
		t.Fatalf("expected promote; got %q (reason: %s)", r.Recommendation, r.RecommendReason)
	}
}

// challenger 跟 champion 等价（相同分数）→ CI 跨 0 → hold
func TestReport_HoldsEquivalentChallenger(t *testing.T) {
	tr := NewABTracker(2000)
	rng := rand.New(rand.NewSource(42))
	outcomes := map[string]bool{}
	for i := 0; i < 200; i++ {
		isFraud := rng.Float64() < 0.3
		score := 0.5*boolToF(isFraud) + 0.5*rng.Float64()
		id := "x" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		tr.Record(id, score, []NamedResult{{Name: "v2", Score: score}})
		outcomes[id] = isFraud
	}
	getOutcome := func(id string) (bool, bool) { v, ok := outcomes[id]; return v, ok }
	rep := tr.Report(getOutcome, 30, 200)
	if len(rep) != 1 {
		t.Fatalf("expected 1 challenger; got %d", len(rep))
	}
	if rep[0].Recommendation != "hold" {
		t.Fatalf("identical scores → hold; got %q (CI [%f, %f])",
			rep[0].Recommendation, rep[0].CILow, rep[0].CIHigh)
	}
}

func TestReport_SkipsLowSampleChallenger(t *testing.T) {
	tr := NewABTracker(50)
	for i := 0; i < 10; i++ {
		id := "lo" + string(rune('0'+i))
		tr.Record(id, 0.5, []NamedResult{{Name: "v2", Score: 0.6}})
	}
	getOutcome := func(id string) (bool, bool) { return true, true }
	// minLabeled=30 → 跳过（< 30 样本）
	rep := tr.Report(getOutcome, 30, 100)
	if len(rep) != 0 {
		t.Fatalf("expected empty report; got %+v", rep)
	}
}

func TestReport_NoBootstrapHoldByDefault(t *testing.T) {
	tr := NewABTracker(200)
	rng := rand.New(rand.NewSource(1))
	outcomes := map[string]bool{}
	for i := 0; i < 50; i++ {
		isFraud := rng.Float64() < 0.5
		id := "z" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		tr.Record(id, rng.Float64(), []NamedResult{{Name: "v2", Score: rng.Float64()}})
		outcomes[id] = isFraud
	}
	getOutcome := func(id string) (bool, bool) { v, ok := outcomes[id]; return v, ok }
	rep := tr.Report(getOutcome, 30, 0)
	if len(rep) == 0 || rep[0].Recommendation != "hold" {
		t.Fatalf("no bootstrap → hold; got %+v", rep)
	}
}

func TestReport_NilTracker(t *testing.T) {
	var tr *ABTracker
	if got := tr.Report(nil, 0, 0); got != nil {
		t.Fatalf("nil tracker should return nil; got %+v", got)
	}
}

func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

package mlscore

import (
	"testing"
)

func TestDrift_NoBaselineMeansNoDrift(t *testing.T) {
	m := NewDriftMonitor(100, 30)
	for i := 0; i < 200; i++ {
		m.Observe(0.5)
	}
	if drifted, _ := m.IsDrifted(); drifted {
		t.Fatal("no baseline should not report drift")
	}
}

func TestDrift_StablePopulationNoAlert(t *testing.T) {
	m := NewDriftMonitor(500, 30)
	for i := 0; i < 500; i++ {
		m.Observe(0.10) // 稳定 0.10
	}
	m.SetBaseline(m.Snapshot())
	for i := 0; i < 500; i++ {
		m.Observe(0.11) // 几乎一样，10% 漂移
	}
	if drifted, reports := m.IsDrifted(); drifted {
		t.Fatalf("10%% drift should not alert (threshold 30%%): %+v", reports)
	}
}

func TestDrift_BigShiftAlerts(t *testing.T) {
	m := NewDriftMonitor(500, 30)
	for i := 0; i < 500; i++ {
		m.Observe(0.10)
	}
	m.SetBaseline(m.Snapshot())
	// 整批切到 0.40 → mean drift = 300%
	for i := 0; i < 500; i++ {
		m.Observe(0.40)
	}
	drifted, reports := m.IsDrifted()
	if !drifted {
		t.Fatalf("massive shift should alert, got reports %+v", reports)
	}
	// 至少 mean 报告 deltaPct > 30
	found := false
	for _, r := range reports {
		if r.Metric == "mean" && r.DeltaPct > 30 {
			found = true
		}
	}
	if !found {
		t.Fatalf("mean drift report missing: %+v", reports)
	}
}

func TestDrift_SmallSampleNoAlert(t *testing.T) {
	// 用大 reservoir 保证总样本数低于 noise floor (100)
	m := NewDriftMonitor(500, 30)
	for i := 0; i < 30; i++ {
		m.Observe(0.10)
	}
	m.SetBaseline(m.Snapshot())
	for i := 0; i < 30; i++ {
		m.Observe(0.99)
	}
	// 当前只有 60 个样本（30+30），不足 100 noise floor
	if drifted, _ := m.IsDrifted(); drifted {
		t.Fatal("< 100 samples should not alert (noise floor)")
	}
}

func TestDrift_SnapshotPercentiles(t *testing.T) {
	m := NewDriftMonitor(100, 30)
	for i := 0; i < 100; i++ {
		m.Observe(float64(i) / 100.0) // 0..0.99
	}
	s := m.Snapshot()
	if s.Count != 100 {
		t.Fatalf("count = %d", s.Count)
	}
	if s.P50 < 0.45 || s.P50 > 0.55 {
		t.Fatalf("p50 ≈ 0.5 expected, got %v", s.P50)
	}
	if s.P95 < 0.90 || s.P95 > 1.0 {
		t.Fatalf("p95 ≈ 0.95 expected, got %v", s.P95)
	}
}

func TestDrift_ReservoirOverflow(t *testing.T) {
	m := NewDriftMonitor(10, 30)
	for i := 0; i < 100; i++ {
		m.Observe(float64(i))
	}
	if got := m.TotalObserved(); got != 100 {
		t.Fatalf("total = %d", got)
	}
	s := m.Snapshot()
	if s.Count != 10 {
		t.Fatalf("reservoir size cap = 10, got count=%d", s.Count)
	}
	// Reservoir 是 FIFO，最近 10 笔是 90..99
	if s.Mean < 90 || s.Mean > 99 {
		t.Fatalf("recent mean ≈ 94.5, got %v", s.Mean)
	}
}

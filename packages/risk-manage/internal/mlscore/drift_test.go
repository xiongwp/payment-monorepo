package mlscore

import (
	"math"
	"math/rand"
	"testing"
)

// ─── 旧 API 兼容性测试（mean shift backup 路径） ──────────────────────────

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
		m.Observe(0.10)
	}
	m.SetBaseline(m.Snapshot())
	for i := 0; i < 500; i++ {
		m.Observe(0.11)
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
	for i := 0; i < 500; i++ {
		m.Observe(0.40)
	}
	drifted, reports := m.IsDrifted()
	if !drifted {
		t.Fatalf("massive shift should alert, got reports %+v", reports)
	}
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
	m := NewDriftMonitor(500, 30)
	for i := 0; i < 30; i++ {
		m.Observe(0.10)
	}
	m.SetBaseline(m.Snapshot())
	for i := 0; i < 30; i++ {
		m.Observe(0.99)
	}
	if drifted, _ := m.IsDrifted(); drifted {
		t.Fatal("< 100 samples should not alert (noise floor)")
	}
}

func TestDrift_SnapshotPercentiles(t *testing.T) {
	m := NewDriftMonitor(100, 30)
	for i := 0; i < 100; i++ {
		m.Observe(float64(i) / 100.0)
	}
	s := m.Snapshot()
	if s.Count != 100 {
		t.Fatalf("count = %d", s.Count)
	}
	if s.P50 < 0.40 || s.P50 > 0.60 {
		t.Fatalf("p50 ≈ 0.5 expected, got %v", s.P50)
	}
}

// ReservoirOverflow：reservoir sampling 后样本是无偏抽样，期望均值收敛
// 到全集均值（49.5）；放宽到一个合理区间。
func TestDrift_ReservoirSamplingUnbiased(t *testing.T) {
	m := NewDriftMonitor(100, 30)
	for i := 0; i < 10000; i++ {
		m.Observe(float64(i % 100))
	}
	if got := m.TotalObserved(); got != 10000 {
		t.Fatalf("total = %d", got)
	}
	s := m.Snapshot()
	if s.Count != 100 {
		t.Fatalf("reservoir size = %d", s.Count)
	}
	// 全集均值 = 49.5；reservoir sampling 应近似落在 30..70 之间（不强一致）
	if s.Mean < 30 || s.Mean > 70 {
		t.Fatalf("reservoir mean = %v, expected ~49.5 ±20", s.Mean)
	}
}

// ─── PSI 测试 ────────────────────────────────────────────────────────────

// 同分布 → PSI ≈ 0（< 0.01 在 n=1000 通常成立，这里放宽到 0.05）。
func TestPSI_IdenticalDistribution(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	a := normalSample(rng, 0, 1, 1000)
	b := normalSample(rng, 0, 1, 1000)
	psi := PSI(a, b, 10)
	if math.IsNaN(psi) {
		t.Fatalf("psi NaN on equal-size samples")
	}
	if psi > 0.05 {
		t.Fatalf("same dist PSI should be ≈ 0, got %v", psi)
	}
}

// 1σ mean shift → PSI ∈ 大致 0.2..1.0（N(0,1) vs N(1,1) 重叠减半左右），
// 行业经验：1σ 是 "明显漂移"。
func TestPSI_OneSigmaShift(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	base := normalSample(rng, 0, 1, 2000)
	cur := normalSample(rng, 1, 1, 2000)
	psi := PSI(base, cur, 10)
	if math.IsNaN(psi) || psi < 0.05 {
		t.Fatalf("1σ shift PSI should clearly fire, got %v", psi)
	}
	if psi < 0.10 {
		t.Logf("warning: 1σ shift PSI=%v sits at warning boundary", psi)
	}
}

// 完全不同分布 → PSI ≫ 0.5。
func TestPSI_CompletelyDifferent(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	base := normalSample(rng, 0, 1, 2000)
	cur := normalSample(rng, 5, 0.3, 2000) // shifted + tighter
	psi := PSI(base, cur, 10)
	if psi < 0.5 {
		t.Fatalf("disjoint dists should give PSI > 0.5, got %v", psi)
	}
}

func TestPSI_InsufficientSamples(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5}
	b := []float64{1, 2, 3, 4, 5}
	if !math.IsNaN(PSI(a, b, 10)) {
		t.Fatal("n<30 should return NaN")
	}
}

func TestPSI_DegenerateBaseline(t *testing.T) {
	// 全相同值 → quantileEdges 返 nil → PSI NaN
	a := make([]float64, 100)
	b := make([]float64, 100)
	for i := range a {
		a[i] = 0.5
		b[i] = 0.5
	}
	if !math.IsNaN(PSI(a, b, 10)) {
		t.Fatal("degenerate baseline should return NaN")
	}
}

// ─── KS 测试 ─────────────────────────────────────────────────────────────

func TestKS_IdenticalDistribution(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	a := normalSample(rng, 0, 1, 500)
	b := normalSample(rng, 0, 1, 500)
	d, p := KSTest(a, b)
	if math.IsNaN(d) {
		t.Fatal("KS NaN on sized samples")
	}
	if p < 0.05 {
		t.Fatalf("same dist KS p should be > 0.05, got d=%v p=%v", d, p)
	}
}

func TestKS_DifferentDistributions(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	a := normalSample(rng, 0, 1, 500)
	b := normalSample(rng, 2, 1, 500)
	d, p := KSTest(a, b)
	if p > 0.01 {
		t.Fatalf("2σ shift KS p should reject, got d=%v p=%v", d, p)
	}
	if d < 0.3 {
		t.Fatalf("KS D should be large, got %v", d)
	}
}

// Distribution shape change with same mean — KS 应该抓到，mean shift 抓不到。
func TestKS_ShapeShiftSameMean(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	// N(0,1) vs uniform(-3, 3)：均值都 ~ 0，但 shape 完全不同
	base := normalSample(rng, 0, 1, 1000)
	cur := uniformSample(rng, -3, 3, 1000)
	d, p := KSTest(base, cur)
	if p > 0.01 {
		t.Fatalf("shape shift should be detected by KS, got d=%v p=%v", d, p)
	}
}

func TestKS_InsufficientSamples(t *testing.T) {
	a := []float64{1, 2, 3}
	b := []float64{4, 5, 6}
	if d, p := KSTest(a, b); !math.IsNaN(d) || !math.IsNaN(p) {
		t.Fatal("n<30 should return NaN")
	}
}

// ─── 多特征独立监控 ────────────────────────────────────────────────────

func TestMultiFeature_IndependentBaseline(t *testing.T) {
	m := NewDriftMonitor(2000, 30)
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 1000; i++ {
		m.ObserveFeatures(map[string]float64{
			"amount":     rng.NormFloat64()*100 + 500,
			"latency_ms": rng.NormFloat64()*50 + 200,
		})
	}
	m.SetBaselineAll()
	// 只让 amount 漂；latency 保持
	for i := 0; i < 1000; i++ {
		m.ObserveFeatures(map[string]float64{
			"amount":     rng.NormFloat64()*100 + 800, // mean shifted
			"latency_ms": rng.NormFloat64()*50 + 200,
		})
	}
	st := m.StatusAll()
	var amt, lat *FeatureDrift
	for i := range st.Features {
		switch st.Features[i].Feature {
		case "amount":
			amt = &st.Features[i]
		case "latency_ms":
			lat = &st.Features[i]
		}
	}
	if amt == nil || lat == nil {
		t.Fatalf("missing feature: amount=%v lat=%v", amt, lat)
	}
	if amt.Verdict == "ok" || amt.PSI < 0.10 {
		t.Fatalf("amount should warn/critical: %+v", amt)
	}
	if lat.Verdict != "ok" {
		t.Fatalf("latency_ms should stay ok: %+v", lat)
	}
}

// SetBaseline 后立刻 PSI ≈ 0（current 就是 baseline 自己）。
func TestSetBaseline_ImmediatePSIZero(t *testing.T) {
	m := NewDriftMonitor(1000, 30)
	rng := rand.New(rand.NewSource(13))
	for i := 0; i < 1000; i++ {
		m.ObserveFeatures(map[string]float64{"x": rng.NormFloat64()})
	}
	m.SetBaselineAll()
	st := m.StatusAll()
	for _, fd := range st.Features {
		if fd.Feature != "x" {
			continue
		}
		if math.IsNaN(fd.PSI) {
			t.Fatalf("PSI NaN right after baseline: %+v", fd)
		}
		if fd.PSI > 0.001 {
			t.Fatalf("PSI right after SetBaseline should be ≈ 0, got %v", fd.PSI)
		}
	}
}

func TestThresholds_SetAndGet(t *testing.T) {
	m := NewDriftMonitor(1000, 30)
	w, c := m.SetThresholds(0.05, 0.20)
	if w != 0.05 || c != 0.20 {
		t.Fatalf("set thresholds got w=%v c=%v", w, c)
	}
	// invalid (warning > critical) → 不变
	w2, c2 := m.SetThresholds(0.5, 0.1)
	if w2 != 0.05 || c2 != 0.20 {
		t.Fatalf("invalid thresholds should be ignored, got w=%v c=%v", w2, c2)
	}
}

// ─── 辅助：分布生成器（local-only，避免引 stat 库）─────────────────────

func normalSample(rng *rand.Rand, mean, std float64, n int) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = rng.NormFloat64()*std + mean
	}
	return out
}

func uniformSample(rng *rand.Rand, lo, hi float64, n int) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = lo + rng.Float64()*(hi-lo)
	}
	return out
}

// ─── benchmarks ──────────────────────────────────────────────────────────

func BenchmarkPSI_10K(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	base := normalSample(rng, 0, 1, 10000)
	cur := normalSample(rng, 0.5, 1, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = PSI(base, cur, 10)
	}
}

func BenchmarkKS_10K(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	base := normalSample(rng, 0, 1, 10000)
	cur := normalSample(rng, 0.5, 1, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = KSTest(base, cur)
	}
}

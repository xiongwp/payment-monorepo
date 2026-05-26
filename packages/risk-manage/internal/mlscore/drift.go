// drift.go: 模型预测分布漂移监控（PSI + KS 双指标）。
//
// 模型部署后最常见的"静默故障"：
//   1. 输入分布变了（商户上了新地区 / 新支付方式 → ip_country / payment_method 分布偏移）
//   2. 输出分布变了（mean score 突然 0.05 → 0.30，是规则推高 risk 还是模型出问题？）
//   3. 模型本身退化（特征工程 bug / 训练数据漂了）
//
// V2 实现要点：
//   - 业界标准 PSI (Population Stability Index) + KS (Kolmogorov-Smirnov 双样本检验)
//   - 每个特征独立 reservoir + baseline，map[featureName]*featureWindow
//   - PSI 公式：Σ (a_i - e_i) * ln(a_i / e_i)，加 1e-6 防 log(0) / div0
//   - 阈值：行业标准 PSI ≥ 0.10 warning，≥ 0.25 critical
//   - KS pvalue 用 Kolmogorov 分布近似 Q_KS(λ) = 2 Σ (-1)^(k-1) e^(-2 k² λ²)
//   - reservoir sampling (Algorithm R，统计无偏)，每特征上限 10000
//   - 24h GC：超过 24h 没观测到的特征自动清掉
//
// 旧版 mean / p50 / p95 ±30% 偏移作为 backup 保留（IsDrifted 同时考虑两者），
// score 的 Snapshot 仍走原样以兼容 cmd/server/main.go 的 Prometheus exporter。
package mlscore

import (
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// scoreFeatureKey score 本身也作为一个"特征"进 PSI 监控；与具体特征 key
// 不冲突（feature 名称不会包含 "__"）。
const scoreFeatureKey = "__score__"

// reservoirCap 每个特征 reservoir 上限。10K 足够 PSI / KS 收敛（KS p-value
// 在 n ≥ 30 即可信，PSI 在 n ≥ 1000 推荐）。
const reservoirCap = 10000

// minSamplesForTest 单边 reservoir 样本下限。< 30 → PSI / KS 返回 NaN
// 标记 "insufficient data"（KS pvalue 近似在小样本不准）。
const minSamplesForTest = 30

// gcAfter 超过此时长没观测到的特征 reservoir 被回收。避免特征改名 / 临时
// 实验特征长期占内存。
const gcAfter = 24 * time.Hour

// DriftMonitor 在线分布漂移监控（多特征版）。线程安全。
//
// 工作流：
//   1. NewDriftMonitor(reservoirSize, driftThresholdPct)
//   2. 每次 Score 完：
//        m.Observe(score)                       // score 本身
//        m.ObserveFeatures(map[string]float64)  // 入参特征
//   3. 周期 (cron / Prometheus scrape) 调 StatusAll() 拿每特征 PSI / KS / verdict
//   4. SetBaselineAll() 在新模型上线 / 周期校准时把当前 → 基线
//
// 兼容旧 API：Observe / Snapshot / Baseline / SetBaseline / IsDrifted /
// TotalObserved 全部基于 scoreFeatureKey 维度。
type DriftMonitor struct {
	mu       sync.Mutex
	features map[string]*featureWindow

	// 配置
	reservoirSize     int
	driftThresholdPct float64 // mean shift backup 阈值（旧逻辑保留）
	psiWarning        float64 // 默认 0.10
	psiCritical       float64 // 默认 0.25
	psiBins           int     // 默认 10

	count int64 // 总观测数（atomic 走单独路径）
}

// featureWindow 单个特征的 reservoir + baseline 快照。
type featureWindow struct {
	mu        sync.Mutex
	samples   []float64 // reservoir 样本
	n         int64     // 该特征累计观测数（用于 reservoir sampling）
	lastObs   time.Time // GC 用
	baseline  []float64 // 基线样本（snapshot 时 deep copy）
	baselined bool
	rng       *rand.Rand
}

// Snapshot 一时刻的 score 分布 stats（兼容旧 API）。
type Snapshot struct {
	Count      int       `json:"count"`
	Mean       float64   `json:"mean"`
	P50        float64   `json:"p50"`
	P95        float64   `json:"p95"`
	P99        float64   `json:"p99"`
	Max        float64   `json:"max"`
	CapturedAt time.Time `json:"captured_at"`
}

// DriftReport 单个指标的漂移情况（兼容旧 API；mean/p50/p95 三条）。
type DriftReport struct {
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	DeltaPct float64 `json:"delta_pct"`
}

// FeatureDrift 单特征的 PSI + KS + verdict。
type FeatureDrift struct {
	Feature     string  `json:"feature"`
	CurrentN    int     `json:"current_n"`
	BaselineN   int     `json:"baseline_n"`
	PSI         float64 `json:"psi"`         // NaN = 样本不足
	KSStatistic float64 `json:"ks_stat"`     // NaN = 样本不足
	KSPValue    float64 `json:"ks_pvalue"`   // NaN = 样本不足
	Verdict     string  `json:"verdict"`     // "ok" | "warning" | "critical" | "no_baseline" | "insufficient"
	LastObsAt   string  `json:"last_obs_at"`
}

// StatusReport 所有特征的当前状态。
type StatusReport struct {
	Features      []FeatureDrift `json:"features"`
	PSIWarning    float64        `json:"psi_warning"`
	PSICritical   float64        `json:"psi_critical"`
	CapturedAt    time.Time      `json:"captured_at"`
	TotalObserved int64          `json:"total_observed"`
}

// NewDriftMonitor 旧签名保留：reservoirSize 用于 backward-compat（per-feature
// 实际 capped at reservoirCap=10000）；driftThresholdPct 控旧 mean/p50/p95
// 偏移阈值（IsDrifted backup 路径）。
func NewDriftMonitor(reservoirSize int, driftThresholdPct float64) *DriftMonitor {
	if reservoirSize <= 0 {
		reservoirSize = 1000
	}
	if reservoirSize > reservoirCap {
		reservoirSize = reservoirCap
	}
	if driftThresholdPct <= 0 {
		driftThresholdPct = 30
	}
	return &DriftMonitor{
		features:          make(map[string]*featureWindow),
		reservoirSize:     reservoirSize,
		driftThresholdPct: driftThresholdPct,
		psiWarning:        0.10,
		psiCritical:       0.25,
		psiBins:           10,
	}
}

// Observe 兼容旧 API：把 score 加到 __score__ 特征。
func (m *DriftMonitor) Observe(score float64) {
	atomic.AddInt64(&m.count, 1)
	m.observeFeature(scoreFeatureKey, score)
}

// ObserveFeatures 批量观测多个特征（一次 Score 内的所有数值型特征）。
// 非数值 / NaN / Inf 应在调用方过滤；这里防御性丢 NaN / Inf。
func (m *DriftMonitor) ObserveFeatures(feats map[string]float64) {
	for k, v := range feats {
		if math.IsNaN(v) || math.IsInf(v, 0) || k == "" {
			continue
		}
		m.observeFeature(k, v)
	}
}

// observeFeature reservoir sampling Algorithm R：
//   - 前 reservoirSize 个直接 append
//   - 之后第 n（1-indexed）个样本以概率 reservoirSize/n 替换随机一个位置
// 统计意义：任何时刻 reservoir 是 i.i.d. 抽样，不像 FIFO 受时间窗影响。
func (m *DriftMonitor) observeFeature(key string, v float64) {
	m.mu.Lock()
	fw, ok := m.features[key]
	if !ok {
		fw = &featureWindow{
			samples: make([]float64, 0, m.reservoirSize),
			// 每特征独立 rng，避免全局锁；seed 用 time + key hash 弱混合够用
			rng: rand.New(rand.NewSource(time.Now().UnixNano() ^ int64(hashStr(key)))),
		}
		m.features[key] = fw
	}
	size := m.reservoirSize
	m.mu.Unlock()

	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.lastObs = time.Now()
	fw.n++
	if len(fw.samples) < size {
		fw.samples = append(fw.samples, v)
		return
	}
	// Algorithm R 替换
	j := fw.rng.Int63n(fw.n)
	if int(j) < size {
		fw.samples[j] = v
	}
}

// hashStr FNV-1a 32-bit；仅给 rng seed 用，不要求安全。
func hashStr(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// Snapshot 兼容旧 API：返回 __score__ 维度的统计快照。
func (m *DriftMonitor) Snapshot() Snapshot {
	m.mu.Lock()
	fw := m.features[scoreFeatureKey]
	m.mu.Unlock()
	if fw == nil {
		return Snapshot{CapturedAt: time.Now().UTC()}
	}
	fw.mu.Lock()
	cp := make([]float64, len(fw.samples))
	copy(cp, fw.samples)
	fw.mu.Unlock()
	return statSnapshot(cp)
}

func statSnapshot(cp []float64) Snapshot {
	if len(cp) == 0 {
		return Snapshot{CapturedAt: time.Now().UTC()}
	}
	sort.Float64s(cp)
	sum := 0.0
	for _, v := range cp {
		sum += v
	}
	n := len(cp)
	return Snapshot{
		Count:      n,
		Mean:       sum / float64(n),
		P50:        cp[n*50/100],
		P95:        cp[min(n*95/100, n-1)],
		P99:        cp[min(n*99/100, n-1)],
		Max:        cp[n-1],
		CapturedAt: time.Now().UTC(),
	}
}

// SetBaseline 兼容旧 API：把当前 __score__ reservoir snapshot 成基线。
// 注意旧签名收 Snapshot 参数（用于 Prometheus exporter 路径）；语义保留为
// "把现在的 score reservoir 作基线"，参数仅用于审计 log（main.go 已 zap）。
//
// 新代码应调 SetBaselineAll() 把所有特征一起 snapshot。
func (m *DriftMonitor) SetBaseline(_ Snapshot) {
	m.snapshotBaseline(scoreFeatureKey)
}

// SetBaselineAll 把当前所有特征 reservoir 复制为各自 baseline。
// 模型新版上线 / 周期校准（如月度）时调。
func (m *DriftMonitor) SetBaselineAll() {
	m.mu.Lock()
	keys := make([]string, 0, len(m.features))
	for k := range m.features {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	for _, k := range keys {
		m.snapshotBaseline(k)
	}
}

func (m *DriftMonitor) snapshotBaseline(key string) {
	m.mu.Lock()
	fw := m.features[key]
	m.mu.Unlock()
	if fw == nil {
		return
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.baseline = make([]float64, len(fw.samples))
	copy(fw.baseline, fw.samples)
	fw.baselined = true
}

// Baseline 兼容旧 API：返回 __score__ 基线的统计快照。
func (m *DriftMonitor) Baseline() Snapshot {
	m.mu.Lock()
	fw := m.features[scoreFeatureKey]
	m.mu.Unlock()
	if fw == nil {
		return Snapshot{}
	}
	fw.mu.Lock()
	if !fw.baselined {
		fw.mu.Unlock()
		return Snapshot{}
	}
	cp := make([]float64, len(fw.baseline))
	copy(cp, fw.baseline)
	fw.mu.Unlock()
	return statSnapshot(cp)
}

// IsDrifted 兼容旧 API：保留 mean / p50 / p95 ±threshold 偏移检测做 backup。
// 同时如果 __score__ PSI ≥ critical，也算 drifted。
//
// 第二个返回是 mean/p50/p95 三条 DriftReport。要拿 PSI / KS 细节请走
// StatusAll()。
func (m *DriftMonitor) IsDrifted() (bool, []DriftReport) {
	m.mu.Lock()
	fw := m.features[scoreFeatureKey]
	threshold := m.driftThresholdPct
	psiCrit := m.psiCritical
	m.mu.Unlock()
	if fw == nil {
		return false, nil
	}
	fw.mu.Lock()
	if !fw.baselined {
		fw.mu.Unlock()
		return false, nil
	}
	curCP := make([]float64, len(fw.samples))
	copy(curCP, fw.samples)
	baseCP := make([]float64, len(fw.baseline))
	copy(baseCP, fw.baseline)
	fw.mu.Unlock()

	cur := statSnapshot(curCP)
	if cur.Count < 100 {
		return false, nil // 旧 noise floor
	}
	base := statSnapshot(baseCP)

	reports := []DriftReport{
		{Metric: "mean", Baseline: base.Mean, Current: cur.Mean},
		{Metric: "p50", Baseline: base.P50, Current: cur.P50},
		{Metric: "p95", Baseline: base.P95, Current: cur.P95},
	}
	hit := false
	out := reports[:0]
	for _, r := range reports {
		denom := r.Baseline
		if denom < 0.01 {
			denom = 0.01
		}
		r.DeltaPct = (r.Current - r.Baseline) / denom * 100
		if r.DeltaPct > threshold || r.DeltaPct < -threshold {
			hit = true
		}
		out = append(out, r)
	}
	// PSI critical 直接判 drifted（不依赖 mean shift）
	if psi := PSI(baseCP, curCP, m.psiBins); !math.IsNaN(psi) && psi >= psiCrit {
		hit = true
	}
	return hit, out
}

// TotalObserved 累计 score 观测数（兼容旧 API）。
func (m *DriftMonitor) TotalObserved() int64 { return atomic.LoadInt64(&m.count) }

// SetThresholds 调 PSI 阈值（admin endpoint）。warning < critical，都 > 0。
// 非法输入忽略，返回当前值。
func (m *DriftMonitor) SetThresholds(warning, critical float64) (float64, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if warning > 0 && critical > 0 && warning < critical {
		m.psiWarning = warning
		m.psiCritical = critical
	}
	return m.psiWarning, m.psiCritical
}

// Thresholds 当前 PSI 阈值。
func (m *DriftMonitor) Thresholds() (warning, critical float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.psiWarning, m.psiCritical
}

// StatusAll 所有特征的 PSI/KS 当前状态。GC 路径在此里跑（顺手）。
func (m *DriftMonitor) StatusAll() StatusReport {
	m.gc(time.Now())

	m.mu.Lock()
	keys := make([]string, 0, len(m.features))
	for k := range m.features {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	psiW, psiC := m.psiWarning, m.psiCritical
	bins := m.psiBins
	m.mu.Unlock()

	out := StatusReport{
		Features:      make([]FeatureDrift, 0, len(keys)),
		PSIWarning:    psiW,
		PSICritical:   psiC,
		CapturedAt:    time.Now().UTC(),
		TotalObserved: atomic.LoadInt64(&m.count),
	}
	for _, k := range keys {
		fd := m.featureStatus(k, bins, psiW, psiC)
		out.Features = append(out.Features, fd)
	}
	return out
}

func (m *DriftMonitor) featureStatus(key string, bins int, psiW, psiC float64) FeatureDrift {
	m.mu.Lock()
	fw := m.features[key]
	m.mu.Unlock()
	fd := FeatureDrift{Feature: key, PSI: math.NaN(), KSStatistic: math.NaN(), KSPValue: math.NaN()}
	if fw == nil {
		fd.Verdict = "no_baseline"
		return fd
	}
	fw.mu.Lock()
	curN := len(fw.samples)
	baseN := len(fw.baseline)
	baselined := fw.baselined
	lastObs := fw.lastObs
	var curCP, baseCP []float64
	if baselined {
		curCP = make([]float64, curN)
		copy(curCP, fw.samples)
		baseCP = make([]float64, baseN)
		copy(baseCP, fw.baseline)
	}
	fw.mu.Unlock()

	fd.CurrentN = curN
	fd.BaselineN = baseN
	if !lastObs.IsZero() {
		fd.LastObsAt = lastObs.UTC().Format(time.RFC3339)
	}
	if !baselined {
		fd.Verdict = "no_baseline"
		return fd
	}
	if curN < minSamplesForTest || baseN < minSamplesForTest {
		fd.Verdict = "insufficient"
		return fd
	}
	fd.PSI = PSI(baseCP, curCP, bins)
	fd.KSStatistic, fd.KSPValue = KSTest(baseCP, curCP)
	switch {
	case math.IsNaN(fd.PSI):
		fd.Verdict = "insufficient"
	case fd.PSI >= psiC:
		fd.Verdict = "critical"
	case fd.PSI >= psiW:
		fd.Verdict = "warning"
	default:
		fd.Verdict = "ok"
	}
	return fd
}

// gc 清掉超过 gcAfter 没观测到的特征。
func (m *DriftMonitor) gc(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, fw := range m.features {
		fw.mu.Lock()
		stale := !fw.lastObs.IsZero() && now.Sub(fw.lastObs) > gcAfter
		fw.mu.Unlock()
		if stale && k != scoreFeatureKey {
			delete(m.features, k)
		}
	}
}

// ─── PSI / KS 数值实现 ────────────────────────────────────────────────────

// PSI Population Stability Index：
//
//	PSI = Σ_i (a_i - e_i) * ln(a_i / e_i)
//
// 其中 a_i, e_i 是第 i 个 bin 在 current / baseline 中的样本占比。bin 边界
// 用 baseline + current 合并的分位数（quantile binning，更稳健，相比等宽 bin
// 不受异常值挤压所有桶）。
//
// 行业经验阈值：
//   - PSI < 0.10  → 稳定（ok）
//   - 0.10 ≤ PSI < 0.25 → 警告（特征分布开始飘）
//   - PSI ≥ 0.25 → 严重（强烈建议重训）
//
// 数值稳定性：
//   - 任意 bin 占比 < epsilon (1e-6) 用 epsilon 替代避 log(0) / 除 0
//   - bins 默认 10；样本太少（< 30）或两边其中一边为空 → 返 NaN
func PSI(baseline, current []float64, bins int) float64 {
	if bins <= 0 {
		bins = 10
	}
	if len(baseline) < minSamplesForTest || len(current) < minSamplesForTest {
		return math.NaN()
	}
	// 用合并样本算分位数边界（quantile binning）。
	merged := make([]float64, 0, len(baseline)+len(current))
	merged = append(merged, baseline...)
	merged = append(merged, current...)
	sort.Float64s(merged)
	edges := quantileEdges(merged, bins)
	if len(edges) < 2 {
		return math.NaN()
	}
	baseCounts := bucketize(baseline, edges)
	curCounts := bucketize(current, edges)
	const eps = 1e-6
	baseN := float64(len(baseline))
	curN := float64(len(current))
	psi := 0.0
	for i := range baseCounts {
		e := float64(baseCounts[i]) / baseN
		a := float64(curCounts[i]) / curN
		if e < eps {
			e = eps
		}
		if a < eps {
			a = eps
		}
		psi += (a - e) * math.Log(a/e)
	}
	return psi
}

// quantileEdges 给定 sorted 样本和 bins，返回 bins+1 个分位点
// （含两端 min / max）。若样本退化（所有值相同）→ 退化成单 bin (len < 2)，
// 调用方按 NaN 处理。
func quantileEdges(sorted []float64, bins int) []float64 {
	if len(sorted) == 0 {
		return nil
	}
	if sorted[0] == sorted[len(sorted)-1] {
		// 全相同；PSI 没意义
		return nil
	}
	edges := make([]float64, 0, bins+1)
	// 两端用 -Inf / +Inf 把异常值兜进首尾 bin，防 current 出现 baseline 外
	// 的值时 bucketize 落到 0 桶塞爆 PSI。
	edges = append(edges, math.Inf(-1))
	n := len(sorted)
	for i := 1; i < bins; i++ {
		// 用 i/bins 分位；线性插值简单粗暴够用
		pos := float64(i) / float64(bins) * float64(n-1)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		var q float64
		if lo == hi {
			q = sorted[lo]
		} else {
			frac := pos - float64(lo)
			q = sorted[lo]*(1-frac) + sorted[hi]*frac
		}
		edges = append(edges, q)
	}
	edges = append(edges, math.Inf(+1))
	// 去重相邻相等边界（如 baseline 大量重复值导致中位数相等）
	dedup := edges[:1]
	for i := 1; i < len(edges); i++ {
		if edges[i] > dedup[len(dedup)-1] {
			dedup = append(dedup, edges[i])
		}
	}
	if len(dedup) < 2 {
		return nil
	}
	return dedup
}

// bucketize 计数每 bin 落入样本数；edges 长度 = bins+1。
func bucketize(samples []float64, edges []float64) []int {
	counts := make([]int, len(edges)-1)
	for _, v := range samples {
		// 二分找 bin index：edges[i] <= v < edges[i+1]
		idx := sort.SearchFloat64s(edges, v) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(counts) {
			idx = len(counts) - 1
		}
		counts[idx]++
	}
	return counts
}

// KSTest 双样本 Kolmogorov-Smirnov 检验。
//
// 返回 (D, p)：
//   - D = sup_x |F1(x) - F2(x)|，两个经验 CDF 的最大差值，∈ [0,1]
//   - p = Q_KS(λ)，其中 λ = (√(n1*n2/(n1+n2)) + 0.12 + 0.11/√(...)) * D
//     使用 Kolmogorov 分布近似（Press et al., Numerical Recipes §14.3.3）：
//        Q_KS(λ) = 2 Σ_{k=1..∞} (-1)^(k-1) exp(-2 k² λ²)
//
// 含义：
//   - p 越小 → 越显著拒绝"两样本同分布"假设（同 mean 也能识别 shape 变化）
//   - D 大但 p 也大 → 样本量太少，证据不足
//
// 小样本 (n < 30) 返 NaN；近似在此区间失真。
func KSTest(baseline, current []float64) (statistic, pvalue float64) {
	if len(baseline) < minSamplesForTest || len(current) < minSamplesForTest {
		return math.NaN(), math.NaN()
	}
	a := make([]float64, len(baseline))
	copy(a, baseline)
	b := make([]float64, len(current))
	copy(b, current)
	sort.Float64s(a)
	sort.Float64s(b)

	// 经典 two-sample KS：归并扫描，在每个 unique x 上比 |F1-F2|。
	// 处理 ties：若 a[i] == b[j]，同时把两边等值连续段都推进，再比 diff
	// （否则会在 tie 中段算出虚假大 diff）。
	n1, n2 := len(a), len(b)
	fn1 := 1.0 / float64(n1)
	fn2 := 1.0 / float64(n2)
	var i, j int
	var cdf1, cdf2 float64
	d := 0.0
	for i < n1 && j < n2 {
		x := a[i]
		if b[j] < x {
			x = b[j]
		}
		// 推进所有 == x 的 a
		for i < n1 && a[i] == x {
			cdf1 += fn1
			i++
		}
		// 推进所有 == x 的 b
		for j < n2 && b[j] == x {
			cdf2 += fn2
			j++
		}
		if diff := math.Abs(cdf1 - cdf2); diff > d {
			d = diff
		}
	}
	// 收尾：一边走完，另一边继续推 cdf 到 1.0；diff 只会单调走向 0/最终值
	// 但若 D 出现在尾部需要继续扫
	for i < n1 {
		cdf1 += fn1
		i++
		if diff := math.Abs(cdf1 - cdf2); diff > d {
			d = diff
		}
	}
	for j < n2 {
		cdf2 += fn2
		j++
		if diff := math.Abs(cdf1 - cdf2); diff > d {
			d = diff
		}
	}
	statistic = d
	en := math.Sqrt(float64(n1*n2) / float64(n1+n2))
	pvalue = kolmogorovQ((en + 0.12 + 0.11/en) * d)
	return
}

// kolmogorovQ Q_KS(λ) = 2 Σ (-1)^(k-1) e^(-2 k² λ²)；λ ≥ 0。
// λ 很小时 → 1；λ 大时 → 0。截断到 100 项足够，提前收敛即停。
func kolmogorovQ(lambda float64) float64 {
	if lambda <= 0 {
		return 1.0
	}
	// Press et al. 数值技巧：λ 很小时数值不稳，用对称变换；这里直接级数够。
	const eps1 = 1e-6
	const eps2 = 1e-16
	a2 := -2.0 * lambda * lambda
	sum := 0.0
	prev := 0.0
	sign := 1.0
	for k := 1; k <= 100; k++ {
		term := sign * math.Exp(a2*float64(k)*float64(k))
		sum += term
		if math.Abs(term) <= eps1*math.Abs(prev) || math.Abs(term) <= eps2*sum {
			return clamp01(2.0 * sum)
		}
		prev = math.Abs(term)
		sign = -sign
	}
	// λ 很小级数收敛慢；返回 1 表 "无证据拒绝 H0"
	return 1.0
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

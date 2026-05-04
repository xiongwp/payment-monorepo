// drift.go: 模型预测分布漂移监控。
//
// 模型部署后最常见的"静默故障"：
//   1. 输入分布变了（商户上了新地区 / 新支付方式 → ip_country / payment_method 分布偏移）
//   2. 输出分布变了（mean score 突然 0.05 → 0.30，是规则推高 risk 还是模型出问题？）
//   3. 模型本身退化（特征工程 bug / 训练数据漂了）
//
// 实现：reservoir 采样最近 N 个 score，计算关键统计量（mean / p50 / p95），
// 跟 reference baseline（启动期 / 周期 reset）对比。超阈值 → Prometheus
// alert + log warning。
//
// 不上 KS / PSI 等正式 drift 测试（需要更多样本和外部库）；这里给一个轻量
// 实时哨兵就够告警自用场景。
package mlscore

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DriftMonitor 在线 score 分布监控。线程安全。
//
// 工作流：
//   1. NewDriftMonitor(reservoirSize=1000)
//   2. 每次 Score 完调 Observe(score)
//   3. 周期 (cron / Prometheus scrape) 调 Snapshot() 拿当前 / 基线 stats
//   4. SetBaseline(snapshot) 在新模型上线 / 周期校准时把当前 → 基线
//
// 漂移指标：
//   - mean_drift_pct = (current.mean - baseline.mean) / max(baseline.mean, 0.01) * 100
//   - p95_drift_pct  = 类比
//   - 任一 > drift_threshold_pct → IsDrifted() 返回 true
type DriftMonitor struct {
	mu        sync.Mutex
	reservoir []float64 // 滑窗采样
	maxSize   int
	cursor    int
	count     int64

	baseline    Snapshot // 基线 stats
	baselineSet atomic.Bool

	// 报警阈值
	driftThresholdPct float64
}

// Snapshot 一时刻的分布 stats。
type Snapshot struct {
	Count      int       `json:"count"`
	Mean       float64   `json:"mean"`
	P50        float64   `json:"p50"`
	P95        float64   `json:"p95"`
	P99        float64   `json:"p99"`
	Max        float64   `json:"max"`
	CapturedAt time.Time `json:"captured_at"`
}

func NewDriftMonitor(reservoirSize int, driftThresholdPct float64) *DriftMonitor {
	if reservoirSize <= 0 {
		reservoirSize = 1000
	}
	if driftThresholdPct <= 0 {
		driftThresholdPct = 30 // 默认 ±30% 触发
	}
	return &DriftMonitor{
		reservoir:         make([]float64, 0, reservoirSize),
		maxSize:           reservoirSize,
		driftThresholdPct: driftThresholdPct,
	}
}

// Observe 把一次 score 加到 reservoir。前 maxSize 笔直接 append；之后用
// 滑动窗口（覆盖最早的 cursor 位置）— 简单 FIFO 不是统计 reservoir，但
// 对漂移检测足够（关心"最近 N 笔"分布）。
func (m *DriftMonitor) Observe(score float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	atomic.AddInt64(&m.count, 1)
	if len(m.reservoir) < m.maxSize {
		m.reservoir = append(m.reservoir, score)
		return
	}
	m.reservoir[m.cursor] = score
	m.cursor = (m.cursor + 1) % m.maxSize
}

// Snapshot 计算当前 reservoir 的 stats。O(N log N)，N ≤ maxSize；周期调，不在主路径。
func (m *DriftMonitor) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reservoir) == 0 {
		return Snapshot{CapturedAt: time.Now().UTC()}
	}
	// 拷贝 + 排序
	cp := make([]float64, len(m.reservoir))
	copy(cp, m.reservoir)
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

// SetBaseline 把 Snapshot 作为基线。新模型上线 / 周期校准时调。
func (m *DriftMonitor) SetBaseline(s Snapshot) {
	m.mu.Lock()
	m.baseline = s
	m.mu.Unlock()
	m.baselineSet.Store(true)
}

// Baseline 当前基线快照。
func (m *DriftMonitor) Baseline() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.baseline
}

// IsDrifted 比较当前 vs 基线，任一关键统计量 |Δ%| > 阈值返 true。
// 基线未设 → false（"还没参考"）。样本数 < 100 → false（避免噪声）。
//
// 第二个返回值是触发漂移的指标名 + Δ% 详情，便于告警。
func (m *DriftMonitor) IsDrifted() (bool, []DriftReport) {
	if !m.baselineSet.Load() {
		return false, nil
	}
	cur := m.Snapshot()
	if cur.Count < 100 {
		return false, nil
	}
	m.mu.Lock()
	base := m.baseline
	threshold := m.driftThresholdPct
	m.mu.Unlock()

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
			denom = 0.01 // 避免基线接近 0 时百分比爆炸
		}
		r.DeltaPct = (r.Current - r.Baseline) / denom * 100
		if r.DeltaPct > threshold || r.DeltaPct < -threshold {
			hit = true
		}
		out = append(out, r)
	}
	return hit, out
}

// DriftReport 单个指标的漂移情况。
type DriftReport struct {
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Current  float64 `json:"current"`
	DeltaPct float64 `json:"delta_pct"`
}

// TotalObserved 累计观测数（atomic 计数；reservoir size 是滑窗）。
func (m *DriftMonitor) TotalObserved() int64 { return atomic.LoadInt64(&m.count) }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

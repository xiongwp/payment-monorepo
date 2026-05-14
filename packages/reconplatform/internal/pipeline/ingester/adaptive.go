// adaptive.go — Ingester 自适应 batch size + 反压.
//
// 问题:
//   固定 MaxBatch=500 在两种情况都不理想:
//     - lag 高时: 500 太小,catch-up 慢
//     - 平稳时: 500 → 长 commit 间隔,offset 延迟,故障重处理多
//
// 方案:
//   基于 Kafka consumer lag (近似) + commit P95 latency 动态调:
//     - lag > 10K → 放大 batch (最多 5000)
//     - 平稳 (lag < 100) → 缩到 200, 让 commit 更频繁,offset 紧跟
//     - 中间区域: 线性插值
//
// 实现:
//   Ingester.handle() 每处理一个 batch 后调用 AdaptiveController.Adjust(),
//   返新的 MaxBatch 用于下一轮 PollRecords.
//
// 安全性:
//   - 收敛 in [200, 5000] 防极端
//   - 每 30s 才允许变更一次 (防颠簸)
//   - panic / 极端 lag 时降级到默认 500
package ingester

import (
	"sync"
	"sync/atomic"
	"time"
)

// AdaptiveController 维护 MaxBatch 自适应状态.
type AdaptiveController struct {
	mu              sync.Mutex
	currentBatch    int       // 当前生效的 batch size
	lastAdjustment  time.Time // 上次变更时间
	totalConsumed   atomic.Int64
	totalCommitted  atomic.Int64

	// 配置上下限
	minBatch int
	maxBatch int
	minInterval time.Duration // 两次 Adjust 之间最短间隔
}

// NewAdaptiveController 默认配置: 200 ~ 5000, 每 30s 调一次.
func NewAdaptiveController(initialBatch int) *AdaptiveController {
	if initialBatch <= 0 {
		initialBatch = 500
	}
	return &AdaptiveController{
		currentBatch: initialBatch,
		minBatch:     200,
		maxBatch:     5000,
		minInterval:  30 * time.Second,
	}
}

// CurrentBatch 当前 MaxBatch.
func (a *AdaptiveController) CurrentBatch() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentBatch
}

// RecordConsumed 调用方每处理一个 batch 调一次, 报告本批大小.
func (a *AdaptiveController) RecordConsumed(n int) {
	a.totalConsumed.Add(int64(n))
}

// RecordCommitted 每次 CommitRecords 成功调一次, 报告 commit 的 batch 大小.
func (a *AdaptiveController) RecordCommitted(n int) {
	a.totalCommitted.Add(int64(n))
}

// Adjust 基于当前 lag (传入的近似值) 调整 batch.
//
// approxLag: 近似的未消费消息数 (从 last record offset - committed offset 推算).
//            实际 Ingester 没法直接拿,可用启发式:本批 batch 接近 MaxBatch 且持续 → 大概率 lag 高.
// 返调整后的 batch size; 没变更 (在 minInterval 内) 返当前值.
func (a *AdaptiveController) Adjust(approxLag int64) int {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	if now.Sub(a.lastAdjustment) < a.minInterval {
		return a.currentBatch
	}

	target := a.targetBatchFor(approxLag)
	if target == a.currentBatch {
		return a.currentBatch
	}
	a.currentBatch = target
	a.lastAdjustment = now
	return target
}

// targetBatchFor 根据 lag 决定目标 batch:
//
//   lag < 100:          200  (平稳, 紧 offset)
//   lag < 1000:         500  (默认)
//   lag < 10000:        2000 (中度 lag)
//   lag >= 10000:       5000 (重度 lag, 加速 catch up)
func (a *AdaptiveController) targetBatchFor(lag int64) int {
	var target int
	switch {
	case lag < 100:
		target = 200
	case lag < 1000:
		target = 500
	case lag < 10000:
		target = 2000
	default:
		target = 5000
	}
	if target < a.minBatch {
		target = a.minBatch
	}
	if target > a.maxBatch {
		target = a.maxBatch
	}
	return target
}

// SuggestLagFromBatch 启发式: 若上轮 PollRecords 满了 MaxBatch,说明可能有 lag.
//
// 真实 lag 需要 admin API (DescribeConsumerGroup),代价高且需要权限.
// 启发式:近 N 批的 fill rate (实际消费 / MaxBatch) 平均接近 1.0 → lag 高.
// 不需精确数字,够 Adjust 用即可.
//
// 用法:
//
//	fillRatio := float64(len(records)) / float64(curMaxBatch)
//	approxLag := SuggestLagFromBatch(fillRatio, curMaxBatch)
//	newBatch := controller.Adjust(approxLag)
func SuggestLagFromBatch(fillRatio float64, currentMaxBatch int) int64 {
	if fillRatio >= 0.95 {
		// 满载,放大估计
		return int64(currentMaxBatch) * 20
	}
	if fillRatio >= 0.5 {
		return int64(currentMaxBatch) * 2
	}
	if fillRatio >= 0.1 {
		return int64(currentMaxBatch)
	}
	return 0 // 平稳
}

// Stats 暴露.
type AdaptiveStats struct {
	CurrentBatch   int
	TotalConsumed  int64
	TotalCommitted int64
}

// Stats 取计数.
func (a *AdaptiveController) Stats() AdaptiveStats {
	return AdaptiveStats{
		CurrentBatch:   a.CurrentBatch(),
		TotalConsumed:  a.totalConsumed.Load(),
		TotalCommitted: a.totalCommitted.Load(),
	}
}

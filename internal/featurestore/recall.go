// recall.go: 从 featurestore.Snapshot + outcome_label 计算系统级风控指标。
//
// 概念：把每一笔风控决策视作"是否 fraud"二分类预测：
//   - 模型预测 fraud == verdict ∈ {DENY, REVIEW}
//   - 模型预测 legit == verdict == ALLOW
//   - 真值（label）来自 OutcomeRecorder 后续 join：chargeback / dispute → fraud=true
//
// 算 confusion matrix 4 格 + Precision / Recall / FPR / F1。
//
// 仅 MemStore 实现暴露此函数（流式 EventbusStore 在 consumer 侧自己算）。
package featurestore

import "time"

// RecallStats 跟 dashboard.RecallStats 字段对齐（dashboard 包 import 本包会循环；
// 调用方手动复制字段或本包再加一个 helper）。
type RecallStats struct {
	WindowDays        int
	LabeledSamples    int
	ActualFraud       int
	ActualLegit       int
	TruePositives     int
	FalsePositives    int
	TrueNegatives     int
	FalseNegatives    int
	PrecisionAtBlock  float64
	RecallAtBlock     float64
	FalsePositiveRate float64
	F1Score           float64
}

// ComputeRecall 从 MemStore 拿最近 N 天 labeled snapshot 算 P/R/FPR。
// 流式后端调用方应自己实现（消费 ml.feature_outcome topic 落 ClickHouse 后跑 SQL）。
func (s *MemStore) ComputeRecall(windowDays int) RecallStats {
	if windowDays <= 0 {
		windowDays = 7
	}
	cutoff := time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour)

	stats := RecallStats{WindowDays: windowDays}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, snap := range s.m {
		if snap.OutcomeLabel == nil {
			continue
		}
		if snap.OccurAt.Before(cutoff) {
			continue
		}
		stats.LabeledSamples++
		isFraud := *snap.OutcomeLabel
		isBlocked := snap.Verdict == "DENY" || snap.Verdict == "REVIEW"
		switch {
		case isFraud && isBlocked:
			stats.TruePositives++
			stats.ActualFraud++
		case isFraud && !isBlocked:
			stats.FalseNegatives++
			stats.ActualFraud++
		case !isFraud && isBlocked:
			stats.FalsePositives++
			stats.ActualLegit++
		default:
			stats.TrueNegatives++
			stats.ActualLegit++
		}
	}
	if stats.TruePositives+stats.FalsePositives > 0 {
		stats.PrecisionAtBlock = float64(stats.TruePositives) /
			float64(stats.TruePositives+stats.FalsePositives)
	}
	if stats.ActualFraud > 0 {
		stats.RecallAtBlock = float64(stats.TruePositives) / float64(stats.ActualFraud)
	}
	if stats.ActualLegit > 0 {
		stats.FalsePositiveRate = float64(stats.FalsePositives) / float64(stats.ActualLegit)
	}
	if stats.PrecisionAtBlock+stats.RecallAtBlock > 0 {
		stats.F1Score = 2 * stats.PrecisionAtBlock * stats.RecallAtBlock /
			(stats.PrecisionAtBlock + stats.RecallAtBlock)
	}
	return stats
}

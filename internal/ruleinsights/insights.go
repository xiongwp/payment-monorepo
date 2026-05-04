// Package ruleinsights 计算每条规则的"运营 KPI"：
//
//	1. last_hit_at      最近一次命中时间。> N 天没命中 = silent rule，
//	                    可能规则被绕过 / 配置过时 / 数据格式变了。
//	2. hits_in_window   滚动窗口内命中次数。看规则火力。
//	3. precision        命中且后续被 outcome 标 fraud=true 的占比。
//	                    低 precision = 高误伤；运营 SOP：< 0.3 调阈值或 shadow 化。
//	4. roi              hits × precision。综合"火力 × 准确度"。给运营按 ROI 排序
//	                    决策："这条规则该留 / 改 / 删"。
//
// 数据源：audit.MemSink + feedback.Recorder。on-demand 计算，不维护单独存储。
// 时间窗口 cap = audit ring 容量；生产应换 ClickHouse 长期统计。
package ruleinsights

import (
	"sort"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

// RuleStats 单条规则的运营快照。
type RuleStats struct {
	RuleID       string    `json:"rule_id"`
	LastHitAt    time.Time `json:"last_hit_at"`
	DaysSinceHit float64   `json:"days_since_hit"`
	IsSilent     bool      `json:"is_silent"`
	HitsInWindow int       `json:"hits_in_window"`
	// Precision/labelCount 仅在样本 >= 5 时填充；样本太少 precision 抖动太大
	// 没有运营价值。
	Precision   float64 `json:"precision,omitempty"`
	LabelCount  int     `json:"label_count"`         // 命中里有 outcome 反馈的样本数
	FraudCount  int     `json:"fraud_count"`         // 其中 is_fraud=true 的
	ROI         float64 `json:"roi"`                 // hits × precision
}

// Compute 跑一次全量统计。
//
// allRuleIDs：runtime 当前在跑的规则 id 全集（即使没命中过也要出现在结果里 —
// 这样运营才能看到 "X 规则 7 天没命中"）。可空 → 仅返回有 hit 的规则。
//
// silenceWindow：超过此时长无命中 → IsSilent=true。典型 7d。
//
// 内部窗口：用 audit MemSink Recent 全量；caller 通过控制 MemSink cap 决定数据范围。
func Compute(audits []*audit.DecisionAudit, fbRec feedback.Recorder, allRuleIDs []string, silenceWindow time.Duration) []RuleStats {
	statsMap := make(map[string]*RuleStats)
	now := time.Now()

	// 用于 precision 计算的临时结构：rule_id → list of decision_ids
	hitsByRule := make(map[string][]string)

	for _, a := range audits {
		if a == nil {
			continue
		}
		for _, h := range a.Hits {
			s, ok := statsMap[h.RuleID]
			if !ok {
				s = &RuleStats{RuleID: h.RuleID}
				statsMap[h.RuleID] = s
			}
			s.HitsInWindow++
			if a.OccurredAt.After(s.LastHitAt) {
				s.LastHitAt = a.OccurredAt
			}
			hitsByRule[h.RuleID] = append(hitsByRule[h.RuleID], a.DecisionID)
		}
	}

	// 把 runtime 已加载但从未命中的规则也补进来（IsSilent 计算需要）。
	for _, id := range allRuleIDs {
		if _, ok := statsMap[id]; !ok {
			statsMap[id] = &RuleStats{RuleID: id}
		}
	}

	// precision：扫每条规则命中的 decision_id，从 feedback 拉 outcome。
	// 多条 outcome 时取 majority（merchant_confirm + dispute 同时存在通常一致）。
	if fbRec != nil {
		for ruleID, decisions := range hitsByRule {
			s := statsMap[ruleID]
			labels := 0
			fraud := 0
			for _, did := range decisions {
				outs := fbRec.Get(did)
				if len(outs) == 0 {
					continue
				}
				labels++
				// 多条 outcome：dispute > merchant_confirm > review_human 优先级
				// （越靠后游越权威）。这里简化：只要任一条 is_fraud=true 就算 fraud
				// — 误伤 vs 真欺诈的判定生产更复杂，本工具只给运营 directional signal。
				for _, o := range outs {
					if o.IsFraud {
						fraud++
						break
					}
				}
			}
			s.LabelCount = labels
			s.FraudCount = fraud
			if labels >= 5 {
				s.Precision = float64(fraud) / float64(labels)
				s.ROI = float64(s.HitsInWindow) * s.Precision
			}
		}
	}

	// silence 计算
	for _, s := range statsMap {
		if s.LastHitAt.IsZero() {
			s.IsSilent = true
			s.DaysSinceHit = -1 // 从未命中标记
		} else {
			s.DaysSinceHit = now.Sub(s.LastHitAt).Hours() / 24
			s.IsSilent = now.Sub(s.LastHitAt) > silenceWindow
		}
	}

	out := make([]RuleStats, 0, len(statsMap))
	for _, s := range statsMap {
		out = append(out, *s)
	}
	// 排序：ROI 降序（让运营一眼看到高价值规则）；ROI=0 的按 hits desc 兜底；
	// 全 0 的按 rule_id 字典序兜底，保证输出稳定。
	sort.Slice(out, func(i, j int) bool {
		if out[i].ROI != out[j].ROI {
			return out[i].ROI > out[j].ROI
		}
		if out[i].HitsInWindow != out[j].HitsInWindow {
			return out[i].HitsInWindow > out[j].HitsInWindow
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

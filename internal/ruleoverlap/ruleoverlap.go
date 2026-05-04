// Package ruleoverlap 算"规则同时命中"矩阵 (co-fire matrix)。
//
// 给运营找：
//   - 规则冗余：A 100% 蕴含 B（A 命中时 B 也总命中）→ 删一个
//   - 规则冲突：A + B 同时高频命中但 verdict 矛盾（DENY vs ALLOW Force）
//   - 规则相关性高：用 Jaccard 相似度 > 0.8 → 看是不是设计意图重复
//
// 实现：on-demand 跑（Compute），从 audit.MemSink Recent 取数据。生产
// 应该用 ClickHouse 长期统计替换计算路径，这里限制在 ring buffer 范围。
package ruleoverlap

import (
	"sort"

	"github.com/xiongwp/risk-manage/internal/audit"
)

// PairStat 单对 (A, B) 的 co-fire 统计。
type PairStat struct {
	RuleA       string  `json:"rule_a"`
	RuleB       string  `json:"rule_b"`
	HitsA       int     `json:"hits_a"`
	HitsB       int     `json:"hits_b"`
	Both        int     `json:"both"`
	Jaccard     float64 `json:"jaccard"`     // |A∩B| / |A∪B|
	AImpliesB   float64 `json:"a_implies_b"` // P(B | A) = both / hits_a
	BImpliesA   float64 `json:"b_implies_a"` // P(A | B) = both / hits_b
	// Verdicts 给前端做冲突检测：A/B 各自常见 verdict（"DENY" / "REVIEW"）
	VerdictA    string  `json:"verdict_a,omitempty"`
	VerdictB    string  `json:"verdict_b,omitempty"`
	IsConflict  bool    `json:"is_conflict"` // both >= minBoth 且 verdict 不一致
}

// Compute 走完 audits 列表算每对规则的 co-fire 统计。
//
// minBoth：两条规则同时命中次数 < minBoth 的不输出（去掉长尾噪音）。
//
// 排序：Jaccard 降序，让最像 / 最重叠的 pair 排前面。
func Compute(audits []*audit.DecisionAudit, minBoth int) []PairStat {
	if minBoth <= 0 {
		minBoth = 5
	}
	// 第一遍：算每条规则的 hit 数 + 主 verdict
	hits := make(map[string]int)
	verdicts := make(map[string]map[string]int) // rule_id → verdict → count
	for _, a := range audits {
		if a == nil {
			continue
		}
		for _, h := range a.Hits {
			hits[h.RuleID]++
			if verdicts[h.RuleID] == nil {
				verdicts[h.RuleID] = map[string]int{}
			}
			verdicts[h.RuleID][h.Decision]++
		}
	}
	// 主 verdict = max count 的那个
	mainVerdict := func(id string) string {
		max := 0
		out := ""
		for v, n := range verdicts[id] {
			if n > max {
				max = n
				out = v
			}
		}
		return out
	}

	// 第二遍：算每对的 both 计数
	type key struct{ a, b string }
	both := make(map[key]int)
	for _, a := range audits {
		if a == nil || len(a.Hits) < 2 {
			continue
		}
		// 去重：同决策同规则只算一次（防恶意 hit 列表）
		seen := make(map[string]bool, len(a.Hits))
		ids := make([]string, 0, len(a.Hits))
		for _, h := range a.Hits {
			if seen[h.RuleID] {
				continue
			}
			seen[h.RuleID] = true
			ids = append(ids, h.RuleID)
		}
		sort.Strings(ids) // 让 (A,B) 跟 (B,A) 共享同 key
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				both[key{ids[i], ids[j]}]++
			}
		}
	}

	out := make([]PairStat, 0, len(both))
	for k, n := range both {
		if n < minBoth {
			continue
		}
		ha, hb := hits[k.a], hits[k.b]
		union := ha + hb - n
		jacc := 0.0
		if union > 0 {
			jacc = float64(n) / float64(union)
		}
		va, vb := mainVerdict(k.a), mainVerdict(k.b)
		conflict := va != "" && vb != "" && va != vb
		stat := PairStat{
			RuleA: k.a, RuleB: k.b,
			HitsA: ha, HitsB: hb, Both: n,
			Jaccard:    jacc,
			AImpliesB:  ifPositive(n, ha),
			BImpliesA:  ifPositive(n, hb),
			VerdictA:   va,
			VerdictB:   vb,
			IsConflict: conflict,
		}
		out = append(out, stat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Jaccard != out[j].Jaccard {
			return out[i].Jaccard > out[j].Jaccard
		}
		// 同 Jaccard 时按 both 降序兜底
		return out[i].Both > out[j].Both
	})
	return out
}

func ifPositive(num, denom int) float64 {
	if denom <= 0 {
		return 0
	}
	return float64(num) / float64(denom)
}

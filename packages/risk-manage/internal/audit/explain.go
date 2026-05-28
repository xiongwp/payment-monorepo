// explain.go: 决策可解释性的 audit 集成。
//
// 把 mlscore.ExplainResult + rule contribution 序列化成可写入 DecisionAudit 的
// ExplainBlock；service 层在 recordAuditWith 主路径后**同步**调一次填充，
// 失败 → log warn 不阻塞（explain 是 nice-to-have，不该卡决策）。
//
// audit ring buffer 已经存了 Hits（rule 命中明细），但没拆每条 rule 的
// score_delta（"这条 rule 贡献了几分"）。RuleContribution 把 engine.Hit
// 升级成带权重 + 顺序的 audit 信息，给 admin /admin/decisions/{id}/explain
// 直接渲染。
//
// 设计：ExplainBlock 是可选字段（omitempty）；老 audit 行没这块时
// admin endpoint 重新算一次（详见 cmd/server registerDecisionExplainHandler）。

package audit

// RuleContribution 单条 rule 在本次决策中的贡献明细。
//
// 跟 AuditHit 互补：AuditHit 是"命中了什么"的事实，RuleContribution 是
// "为什么这条 rule 影响 verdict"的解释——带 sequence（评估顺序）+
// score_delta（按规则类型 / weight 给 RiskScore 的增量）。
type RuleContribution struct {
	RuleID     string  `json:"rule_id"`
	RuleName   string  `json:"rule_name,omitempty"`
	Hit        bool    `json:"hit"`                  // 命中 / 没命中（shadow 命中也 = true）
	Weight     float64 `json:"weight,omitempty"`     // rule 配置 weight；deny/allow 强制规则 = 0
	ScoreDelta int     `json:"score_delta,omitempty"` // 对最终 RiskScore 的增量（命中才 > 0）
	Sequence   int     `json:"sequence"`             // 评估顺序（从 1 开始），便于回放
	Detail     string  `json:"detail,omitempty"`
}

// ExplainBlock 一次决策的完整解释。落到 DecisionAudit.Explain。
//
//   - MLBaseValue / MLFinalScore：训练集均值 logit + 当前样本 sigmoid 后概率
//   - MLTopContributors：top-K 贡献特征（按 |contribution| 排序）
//   - MLAllContributions：全量 feature → contribution 字典（debug 用）
//   - MLModelVersion：解释时用的模型版本（跟 DecisionAudit.MLModelVer 同步）
//   - RuleContributions：每条参与评估的 rule 的贡献
//   - Generated：是否成功生成；失败 → 字段全空 + Error 写原因
//
// 序列化体积控制：AllContributions 通常 < 20 个特征 × 8B = 160B，
// TopContributors top-5 ≈ 300B，整个 block 在 audit JSON 里 < 1KB。
type ExplainBlock struct {
	MLBaseValue         float64                       `json:"ml_base_value"`
	MLFinalScore        float64                       `json:"ml_final_score"`
	MLTopContributors   []ExplainFeatureContribution  `json:"ml_top_contributors,omitempty"`
	MLAllContributions  map[string]float64            `json:"ml_all_contributions,omitempty"`
	MLModelVersion      string                        `json:"ml_model_version,omitempty"`
	RuleContributions   []RuleContribution            `json:"rule_contributions,omitempty"`
	Generated           bool                          `json:"generated"`
	Error               string                        `json:"error,omitempty"`
}

// ExplainFeatureContribution 跟 mlscore.FeatureContribution 字段对齐。
// 重复定义而非 import：避免 audit ← mlscore 反向依赖（mlscore 已经依赖
// audit 在某些 site 行不通），保持 audit 包零业务依赖。
type ExplainFeatureContribution struct {
	Feature      string  `json:"feature"`
	Value        float64 `json:"value"`
	Contribution float64 `json:"contribution"`
	Direction    string  `json:"direction"`
}

// BuildRuleContributions 从 Hits + ShadowHits 拼 RuleContribution 列表。
//
// 评估顺序按入参列表的位置；shadow 命中也算 hit=true，但 score_delta 缺省
// 0（shadow 不影响 RiskScore）。weight 字段需要 caller（service 层）从
// engine 拿 rule 配置填进来；这里只做 shape 转换。
func BuildRuleContributions(hits, shadowHits []AuditHit, weights map[string]float64, deltas map[string]int) []RuleContribution {
	out := make([]RuleContribution, 0, len(hits)+len(shadowHits))
	seq := 1
	for _, h := range hits {
		out = append(out, RuleContribution{
			RuleID:     h.RuleID,
			RuleName:   h.RuleName,
			Hit:        true,
			Weight:     weights[h.RuleID],
			ScoreDelta: deltas[h.RuleID],
			Sequence:   seq,
			Detail:     h.Detail,
		})
		seq++
	}
	for _, h := range shadowHits {
		out = append(out, RuleContribution{
			RuleID:   h.RuleID,
			RuleName: h.RuleName,
			Hit:      true,
			Weight:   weights[h.RuleID],
			// shadow 命中不影响 RiskScore，score_delta 留 0
			Sequence: seq,
			Detail:   h.Detail + " [shadow]",
		})
		seq++
	}
	return out
}

// AttachExplain 把 ExplainBlock 写入 DecisionAudit。
// nil-safe：a == nil → no-op；block == nil → no-op。
func AttachExplain(a *DecisionAudit, block *ExplainBlock) {
	if a == nil || block == nil {
		return
	}
	a.Explain = block
}

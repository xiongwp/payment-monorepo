// Package rulesim 跑"假如这条规则上线了，最近 N 笔决策会怎么变？"
//
// 给 admin-web 规则编辑流程用：运营写完候选规则 → 点 "试运行" → 看到
//   - 候选规则会命中多少笔（覆盖率）
//   - 命中里有多少跟当前 verdict 一致 / 不一致（有多少新增 BLOCK / 改变 verdict）
//   - 命中里有多少有 outcome 反馈 + 真实 fraud 占比（precision 估算）
// 再决定要不要 push 到 enforce。直接削减"先 shadow 一周"的等待时间。
//
// 限制：从 audit.AuditInput 重建 TxnContext 时只有业务字段（merchant /
// customer / amount / country / ip / device 等），SDK-side 的 fingerprint /
// behavior / IP-intel 信号缺失。依赖这些信号的规则在 simulator 里会全 miss，
// 应该放回 shadow 模式跑 N 天再上线。文档跟 Result.Notes 都会标这个 caveat。
package rulesim

import (
	"context"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/feedback"
)

// Result 单次模拟输出。
type Result struct {
	Sample              int     `json:"sample"`                // 重放的 audit 数
	Hits                int     `json:"hits"`                  // 候选规则命中次数
	HitRate             float64 `json:"hit_rate"`              // hits / sample
	WouldNewlyBlock     int     `json:"would_newly_block"`     // 候选 hit + 当前 verdict=ALLOW
	WouldKeepBlock      int     `json:"would_keep_block"`      // 候选 hit + 当前 verdict ∈ {DENY,REVIEW}
	HitsWithOutcome     int     `json:"hits_with_outcome"`     // hit 且有 outcome 反馈的样本
	HitsTrueFraud       int     `json:"hits_true_fraud"`       // 反馈里 is_fraud=true
	EstimatedPrecision  float64 `json:"estimated_precision"`   // = HitsTrueFraud / HitsWithOutcome；要求 >= 5 样本
	Notes               []string `json:"notes,omitempty"`      // 用户提示（如 "依赖 SDK 信号 → 估值偏低"）
}

// Simulate 用候选规则 r 重放 audits 列表，返回汇总结果。
//
// fbRec 用来反查 outcome（precision 估算）；nil 时跳过那一段。
//
// caveat: 见 package doc。本函数不分辨 rule 类型，依赖 SDK 信号的规则会得到
// 偏低的 hit 率。运营 SOP 是 simulate 数字 = 下界，真实表现至少不差。
func Simulate(ctx context.Context, r engine.Rule, audits []*audit.DecisionAudit, fbRec feedback.Recorder) Result {
	res := Result{Sample: 0}
	if r == nil || len(audits) == 0 {
		return res
	}
	for _, a := range audits {
		if a == nil {
			continue
		}
		res.Sample++
		txn := txnFromAudit(a)
		hit := r.Evaluate(ctx, txn)
		if hit == nil {
			continue
		}
		res.Hits++
		// 当前 verdict 是 BLOCK 还是 ALLOW？
		if a.Verdict == "DENY" || a.Verdict == "REVIEW" {
			res.WouldKeepBlock++
		} else {
			res.WouldNewlyBlock++
		}
		// outcome 反馈
		if fbRec == nil {
			continue
		}
		outs := fbRec.Get(a.DecisionID)
		if len(outs) == 0 {
			continue
		}
		res.HitsWithOutcome++
		for _, o := range outs {
			if o.IsFraud {
				res.HitsTrueFraud++
				break
			}
		}
	}
	if res.Sample > 0 {
		res.HitRate = float64(res.Hits) / float64(res.Sample)
	}
	if res.HitsWithOutcome >= 5 {
		res.EstimatedPrecision = float64(res.HitsTrueFraud) / float64(res.HitsWithOutcome)
	}
	if res.HitsWithOutcome < 5 && res.Hits > 0 {
		res.Notes = append(res.Notes,
			"hit 样本里 outcome 反馈 < 5 条；precision 不可估，建议先 shadow 跑 1 周再判断")
	}
	if res.Hits == 0 && res.Sample > 0 {
		res.Notes = append(res.Notes,
			"候选规则零命中。可能：1) 阈值太严；2) 规则依赖 SDK 信号（fingerprint/behavior/IP-intel）— audit 数据没存这些字段，simulator 会低估 hit 率，建议放 shadow 模式跑实际流量")
	}
	return res
}

// txnFromAudit 从 audit 重建 TxnContext。仅业务字段；SDK-side 信号置零。
func txnFromAudit(a *audit.DecisionAudit) *engine.TxnContext {
	return &engine.TxnContext{
		PaymentIntentID: a.Input.PaymentIntentID,
		MerchantID:      a.Input.MerchantID,
		CustomerID:      a.Input.CustomerID,
		Amount:          a.Input.Amount,
		Currency:        a.Input.Currency,
		PaymentMethod:   a.Input.PaymentMethod,
		Country:         a.Input.Country,
		IPAddress:       a.Input.IPAddress,
		DeviceID:        a.Input.DeviceID,
	}
}

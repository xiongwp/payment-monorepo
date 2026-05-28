// chain.go: feedback.Recorder 的 tamper-evident wrapper。
//
// 每条 Outcome.Record 同步上链（stream="outcomes"）。outcome 一旦写入就**不可
// 改 / 不可删**（追加语义）；任何后期 mutation 通过 chain verify 立刻暴露。
//
// 为什么 outcome 要 chain：outcome 是 ML 训练 + 规则迭代的 "正确答案" 来源
// （ML team 拿 outcomes ∪ audit 做 label 训练下一版模型）。如果有人改了
// outcome → ML 模型被毒化 → 风控决策被绕过。属于 supply-chain 攻击面。
//
// chain 独立链（stream="outcomes"），跟其它 stream 互不影响。
//
// record schema：
//
//	{
//	  "type":        "outcome",
//	  "decision_id":"<id>",
//	  "source":     "review_human" | "dispute" | "merchant_confirm",
//	  "is_fraud":   true | false,
//	  "actor":      "<who>",
//	  "notes":      "<free text>",
//	  "occurred_at":"<RFC3339-utc>"
//	}
//
// 失败兜底：chain 写失败不阻塞 inner Record；inner 的 err 透传给 caller。
package feedback

import (
	"context"
	"time"

	"github.com/xiongwp/risk-manage/internal/audit"
	"github.com/xiongwp/risk-manage/internal/metrics"
)

// chainRecorder wraps Recorder，每条 Record 同步追加 chain record。
type chainRecorder struct {
	Recorder
	cw *audit.ChainWriter
}

// WrapWithChain 把 Recorder 包成 chain-aware。cw==nil 或 inner==nil → 返回
// 原 recorder（cfg 关闭时零开销）。
func WrapWithChain(r Recorder, cw *audit.ChainWriter) Recorder {
	if cw == nil || r == nil {
		return r
	}
	return &chainRecorder{Recorder: r, cw: cw}
}

func (c *chainRecorder) Record(o Outcome) error {
	// 先 inner.Record（business persistence），再上链。chain 失败不回滚业务。
	innerErr := c.Recorder.Record(o)

	occurredAt := o.At
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	body := map[string]any{
		"type":        "outcome",
		"decision_id": o.DecisionID,
		"source":      string(o.Source),
		"is_fraud":    o.IsFraud,
		"actor":       o.Actor,
		"notes":       o.Notes,
		"occurred_at": occurredAt.UTC().Format(time.RFC3339Nano),
	}
	if err := c.cw.Append(context.Background(), body); err != nil {
		metrics.AuditChainWriteTotal.WithLabelValues("outcomes", "err").Inc()
	} else {
		metrics.AuditChainWriteTotal.WithLabelValues("outcomes", "ok").Inc()
	}
	return innerErr
}

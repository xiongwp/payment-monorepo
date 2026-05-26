// behavior_lstm_threshold.go — 基于 LSTM 输出的行为异常分数触发规则。
//
// 输入：TxnContext.BehaviorAnomalyScore（[0,1]，0=人类，1=自动化）— 由
// service/risk.go 的 behavior_lstm stage 写入；模型未启用时为 0.5（中性）。
//
// 触发逻辑：
//   - score < threshold → 不命中
//   - score >= threshold → 输出 Hit，Decision 由 spec 决定（REVIEW / DENY）
//
// 用法举例（rule_spec.json）：
//
//	{
//	  "id": "behavior_lstm_high",
//	  "type": "behavior_lstm_threshold",
//	  "enabled": true,
//	  "spec": {
//	    "threshold": 0.7,
//	    "decision": "REVIEW",
//	    "min_signal_quality": 0.5
//	  }
//	}
//
// min_signal_quality：行为数据不足（如鼠标 < 10 事件）时模型 score 不可信，
// 跳过规则避免误杀。0.5 = "至少一半采集 ok"。
package rules

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// behaviorLSTMRule 实现 engine.Rule + WeightedRule。
type behaviorLSTMRule struct {
	id        string
	enabled   bool
	threshold float64
	minQual   float64
	decision  string // "REVIEW" / "DENY"
}

type behaviorLSTMSpec struct {
	Threshold        float64 `json:"threshold"`
	Decision         string  `json:"decision"`
	MinSignalQuality float64 `json:"min_signal_quality"`
}

// NewBehaviorLSTMRule 用 spec JSON 构造规则。
func NewBehaviorLSTMRule(id string, enabled bool, specJSON []byte) (engine.Rule, error) {
	var spec behaviorLSTMSpec
	if len(specJSON) > 0 {
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return nil, err
		}
	}
	if spec.Threshold == 0 {
		spec.Threshold = 0.7 // 行业经验：> 0.7 高置信自动化
	}
	if spec.Threshold < 0 || spec.Threshold > 1 {
		return nil, errors.New("behavior_lstm_threshold: threshold must be in [0,1]")
	}
	if spec.Decision == "" {
		spec.Decision = "REVIEW" // 默认走人工，不直接 deny
	}
	if spec.Decision != "REVIEW" && spec.Decision != "DENY" {
		return nil, errors.New("behavior_lstm_threshold: decision must be REVIEW or DENY")
	}
	if spec.MinSignalQuality <= 0 {
		spec.MinSignalQuality = 0.3
	}
	return &behaviorLSTMRule{
		id:        id,
		enabled:   enabled,
		threshold: spec.Threshold,
		minQual:   spec.MinSignalQuality,
		decision:  spec.Decision,
	}, nil
}

func (r *behaviorLSTMRule) ID() string      { return r.id }
func (r *behaviorLSTMRule) Name() string    { return "behavior_lstm_threshold" }
func (r *behaviorLSTMRule) Type() string    { return "behavior_lstm_threshold" }
func (r *behaviorLSTMRule) Enabled() bool   { return r.enabled }
func (r *behaviorLSTMRule) Weight() int     { return 25 } // 高权重：是 ML 直接判断
func (r *behaviorLSTMRule) Tags() []string  { return []string{"ml", "behavior", "anti-bot"} }

func (r *behaviorLSTMRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if !r.enabled || txn == nil {
		return nil
	}
	// 信号质量不足跳过 — 避免在没数据的情况下用中性 0.5 误判
	if txn.SignalCoverageRatio < r.minQual {
		return nil
	}
	if txn.BehaviorAnomalyScore < r.threshold {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.Name(),
		Decision: r.decision,
		Detail: map[string]interface{}{
			"behavior_anomaly_score": txn.BehaviorAnomalyScore,
			"threshold":              r.threshold,
			"signal_coverage_ratio":  txn.SignalCoverageRatio,
		},
	}
}

package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── ml_threshold: 用 ML 推理分数做阈值判定 ─────────────────────────────
//
// service.Screen 入口调 mlscore.Service.Score → txn.MLScore 填充。本规则
// 按阈值映射成 verdict。
//
// 配置：
//
//	type: ml_threshold
//	config:
//	  threshold:  0.7      # 严格 > 阈值 命中
//	  decision:   review   # review / deny；默认 review
//	  fail_open:  true     # MLScore == 0（视作模型不可用）时是否放行
//	                       # 默认 true：ML 故障不阻断流量

type MLThresholdConfig struct {
	Threshold float64 `json:"threshold"`
	Decision  string  `json:"decision"`
	FailOpen  *bool   `json:"fail_open,omitempty"` // 可选；nil → true
}

type mlThresholdRule struct {
	id, name string
	enabled  bool
	cfg      MLThresholdConfig
	verdict  engine.Decision
	failOpen bool
}

func MLThresholdFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg MLThresholdConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("ml_threshold config: %w", err)
		}
		if cfg.Threshold <= 0 || cfg.Threshold > 1 {
			return nil, fmt.Errorf("ml_threshold: threshold must be in (0, 1], got %v", cfg.Threshold)
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		failOpen := true
		if cfg.FailOpen != nil {
			failOpen = *cfg.FailOpen
		}
		return &mlThresholdRule{
			id: id, name: name, enabled: enabled, cfg: cfg,
			verdict:  verdict,
			failOpen: failOpen,
		}, nil
	}
}

func (r *mlThresholdRule) ID() string    { return r.id }
func (r *mlThresholdRule) Name() string  { return r.name }
func (r *mlThresholdRule) Type() string  { return "ml_threshold" }
func (r *mlThresholdRule) Enabled() bool { return r.enabled }

func (r *mlThresholdRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	// MLScore == 0 视作模型不可用：fail_open=true（默认）→ 不命中；
	// false 时按"模型不可用"看作高风险 → 命中。
	if txn.MLScore == 0 {
		if r.failOpen {
			return nil
		}
		return &engine.Hit{
			RuleID:   r.id,
			RuleName: r.name,
			Decision: r.verdict,
			Detail:   "ML model unavailable (score=0), fail_close",
		}
	}
	if txn.MLScore > r.cfg.Threshold {
		return &engine.Hit{
			RuleID:   r.id,
			RuleName: r.name,
			Decision: r.verdict,
			Detail: fmt.Sprintf("ml_score %.3f > threshold %.3f (model %s)",
				txn.MLScore, r.cfg.Threshold, txn.MLModelVer),
		}
	}
	return nil
}

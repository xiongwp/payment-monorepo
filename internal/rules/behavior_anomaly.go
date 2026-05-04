package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
)

// ─── behavior_anomaly: 行为信号异常检测 ─────────────────────────────
//
// 利用端 SDK 上报的行为字段（TimeToCheckoutMs / MouseMovementEntropy /
// ClickIntervalMs / TypingRhythmCV / KeystrokeCount / PastedFields）识别
// 自动化脚本 / 卡号库批量测试。每条阈值都是可选的（零值 = 不启用此子检查）。
//
// 配置：
//
//	type: behavior_anomaly
//	config:
//	  min_time_to_checkout_ms: 3000   # 提交速度过快阈值（ms）；< 阈值 → 命中
//	  min_mouse_entropy:       0.5    # 鼠标轨迹熵下限；< 阈值 → 命中
//	  bot_click_interval_ms:   50     # 平均点击间隔上限（ms）；< 阈值 → 命中（bot 极快或固定）
//	  max_typing_rhythm_cv:    0.05   # 打字节奏 CV 下限；< 阈值（接近恒定）→ bot 命中
//	  card_paste_blacklist:    true   # PastedFields 含 card_number / cvc → 命中（卡号库信号）
//	  decision:                review # review / deny；默认 review
//
// 至少一个子检查触发即命中（OR）。Detail 列出所有命中原因。

type BehaviorAnomalyConfig struct {
	MinTimeToCheckoutMs int     `json:"min_time_to_checkout_ms"`
	MinMouseEntropy     float64 `json:"min_mouse_entropy"`
	BotClickIntervalMs  int     `json:"bot_click_interval_ms"`
	MaxTypingRhythmCV   float64 `json:"max_typing_rhythm_cv"`
	CardPasteBlacklist  bool    `json:"card_paste_blacklist"`
	Decision            string  `json:"decision"` // review / deny；默认 review
}

type behaviorAnomalyRule struct {
	id, name string
	enabled  bool
	cfg      BehaviorAnomalyConfig
	verdict  engine.Decision
}

func BehaviorAnomalyFactory() engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg BehaviorAnomalyConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("behavior_anomaly config: %w", err)
		}
		verdict := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			verdict = engine.Deny
		}
		return &behaviorAnomalyRule{id: id, name: name, enabled: enabled, cfg: cfg, verdict: verdict}, nil
	}
}

func (r *behaviorAnomalyRule) ID() string    { return r.id }
func (r *behaviorAnomalyRule) Name() string  { return r.name }
func (r *behaviorAnomalyRule) Type() string  { return "behavior_anomaly" }
func (r *behaviorAnomalyRule) Enabled() bool { return r.enabled }

func (r *behaviorAnomalyRule) Evaluate(_ context.Context, txn *engine.TxnContext) *engine.Hit {
	if txn == nil {
		return nil
	}
	var reasons []string

	// 提交速度过快
	if r.cfg.MinTimeToCheckoutMs > 0 && txn.TimeToCheckoutMs > 0 &&
		txn.TimeToCheckoutMs < int64(r.cfg.MinTimeToCheckoutMs) {
		reasons = append(reasons, fmt.Sprintf("time_to_checkout %dms < min %dms",
			txn.TimeToCheckoutMs, r.cfg.MinTimeToCheckoutMs))
	}
	// 鼠标轨迹熵过低（直线 / 静止 / bot）
	if r.cfg.MinMouseEntropy > 0 && txn.MouseMovementEntropy > 0 &&
		txn.MouseMovementEntropy < r.cfg.MinMouseEntropy {
		reasons = append(reasons, fmt.Sprintf("mouse entropy %.2f < min %.2f",
			txn.MouseMovementEntropy, r.cfg.MinMouseEntropy))
	}
	// 点击间隔过短（极速 bot）
	if r.cfg.BotClickIntervalMs > 0 && txn.ClickIntervalMs > 0 &&
		txn.ClickIntervalMs < r.cfg.BotClickIntervalMs {
		reasons = append(reasons, fmt.Sprintf("click_interval %dms < %dms (bot-like)",
			txn.ClickIntervalMs, r.cfg.BotClickIntervalMs))
	}
	// 打字节奏 CV 过低（接近恒定 = 自动键入）
	if r.cfg.MaxTypingRhythmCV > 0 && txn.TypingRhythmCV > 0 &&
		txn.TypingRhythmCV < r.cfg.MaxTypingRhythmCV {
		reasons = append(reasons, fmt.Sprintf("typing rhythm CV %.3f < %.3f (constant intervals)",
			txn.TypingRhythmCV, r.cfg.MaxTypingRhythmCV))
	}
	// 粘贴卡号 / CVC（卡号库 / 拖库信号）
	if r.cfg.CardPasteBlacklist {
		var pastedSensitive []string
		for _, f := range txn.PastedFields {
			lf := strings.ToLower(strings.TrimSpace(f))
			if lf == "card_number" || lf == "cvc" || lf == "cvv" || lf == "card_cvv" {
				pastedSensitive = append(pastedSensitive, lf)
			}
		}
		if len(pastedSensitive) > 0 {
			reasons = append(reasons, "sensitive fields pasted: "+strings.Join(pastedSensitive, ","))
		}
	}

	if len(reasons) == 0 {
		return nil
	}
	return &engine.Hit{
		RuleID:   r.id,
		RuleName: r.name,
		Decision: r.verdict,
		Detail:   strings.Join(reasons, "; "),
	}
}

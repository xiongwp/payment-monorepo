package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── rolling_amount: 滚动 N 天累计金额限制 ─────────────────────────────
//
// 用途：捕获"长尾累计"型 fraud — 单笔金额合规、velocity_amount 短窗也合规，
// 但拉到 7 天 / 30 天周期看就明显偏离正常分布。例如：
//
//   - 用户日均 ¥500，连续 30 天每天 ¥4000（仍在 daily_limit ¥5000 之下）
//     → 30 天累计 12 万 → 命中本规则。
//
// 配置：
//
//	type: rolling_amount
//	config:
//	  scope_by:    customer       # merchant / customer / ip / device / card
//	  days:        7              # 滚动窗口天数（含今天，UTC 自然日聚合）
//	  max_amount:  10000000       # 10 万 minor unit
//	  method:      ""             # 限定支付方式（空=全部）
//	  decision:    review         # review / deny
//
// 与 velocity_amount 的区别：
//   - velocity_amount 用 ZSet ms 级精度，窗口 ≤ 60min；适合突发/拆单 fraud
//   - rolling_amount 用日级 daily 计数加和，窗口 1-30d；适合长尾累计 fraud

type RollingAmountConfig struct {
	ScopeBy   string `json:"scope_by"`
	Days      int    `json:"days"`
	MaxAmount int64  `json:"max_amount"`
	Method    string `json:"method"`
	Decision  string `json:"decision"`
}

type rollingAmountRule struct {
	id, name string
	enabled  bool
	cfg      RollingAmountConfig
	counter  store.Counter
	verdict  engine.Decision
}

func RollingAmountFactory(c store.Counter) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg RollingAmountConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("rolling_amount: %w", err)
		}
		if cfg.Days <= 0 || cfg.MaxAmount <= 0 || cfg.ScopeBy == "" {
			return nil, fmt.Errorf("rolling_amount: scope_by/days/max_amount required")
		}
		// 30d 上限：超过没意义且 Redis 实现保留期通常 30d
		if cfg.Days > 30 {
			cfg.Days = 30
		}
		v := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			v = engine.Deny
		}
		return &rollingAmountRule{id: id, name: name, enabled: enabled, cfg: cfg, counter: c, verdict: v}, nil
	}
}

func (r *rollingAmountRule) ID() string    { return r.id }
func (r *rollingAmountRule) Name() string  { return r.name }
func (r *rollingAmountRule) Type() string  { return "rolling_amount" }
func (r *rollingAmountRule) Enabled() bool { return r.enabled }

func (r *rollingAmountRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.counter == nil || txn == nil {
		return nil
	}
	if r.cfg.Method != "" && txn.PaymentMethod != r.cfg.Method {
		return nil
	}
	key := scopeKey(r.cfg.ScopeBy, txn)
	if key == "" {
		return nil
	}
	sum := r.counter.GetRollingDays(ctx, key, r.cfg.Days)
	// 加上当前 txn amount（Report 还没把这笔写进 counter）
	sum += txn.Amount
	if sum < r.cfg.MaxAmount {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("rolling_amount %s=%s sum=%d in %dd (limit %d)",
			r.cfg.ScopeBy, key, sum, r.cfg.Days, r.cfg.MaxAmount),
	}
}

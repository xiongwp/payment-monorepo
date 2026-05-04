package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── velocity_amount: 滑窗内累计金额 限制 ───────────────────────────
//
// 资损修复 P1-1：counter 实现 AtomicCounter 时走 IncrIfBelowVelocityAmount
// 原子预扣；规则命中即"占位"，避免 GetVelocityAmount + 比较 + Report Incr
// 之间的 race。

type VelocityAmountConfig struct {
	ScopeBy   string `json:"scope_by"`
	WindowMin int    `json:"window_min"`
	MaxAmount int64  `json:"max_amount"`
	Method    string `json:"method"`
	Decision  string `json:"decision"`
}

type velocityAmountRule struct {
	id, name string
	enabled  bool
	cfg      VelocityAmountConfig
	counter  store.Counter
	verdict  engine.Decision
}

func VelocityAmountFactory(c store.Counter) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg VelocityAmountConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("velocity_amount: %w", err)
		}
		if cfg.WindowMin <= 0 || cfg.MaxAmount <= 0 || cfg.ScopeBy == "" {
			return nil, fmt.Errorf("velocity_amount: scope_by/window_min/max_amount required")
		}
		v := engine.Review
		if strings.EqualFold(strings.TrimSpace(cfg.Decision), "deny") {
			v = engine.Deny
		}
		return &velocityAmountRule{id: id, name: name, enabled: enabled, cfg: cfg, counter: c, verdict: v}, nil
	}
}

func (r *velocityAmountRule) ID() string    { return r.id }
func (r *velocityAmountRule) Name() string  { return r.name }
func (r *velocityAmountRule) Type() string  { return "velocity_amount" }
func (r *velocityAmountRule) Enabled() bool { return r.enabled }

func (r *velocityAmountRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
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

	ac, atomicOK := r.counter.(store.AtomicCounter)
	tracker := store.ReservationFromContext(ctx)
	if atomicOK && tracker != nil {
		newSum, ok, _ := ac.IncrIfBelowVelocityAmount(ctx, key, txn.Amount, r.cfg.MaxAmount, r.cfg.WindowMin)
		if !ok {
			return &engine.Hit{
				RuleID: r.id, RuleName: r.name, Decision: r.verdict,
				Detail: fmt.Sprintf("velocity_amount %s=%s sum=%d in %dmin (limit %d, atomic)",
					r.cfg.ScopeBy, key, newSum, r.cfg.WindowMin, r.cfg.MaxAmount),
			}
		}
		tracker.Add(store.Reservation{
			Kind: store.ReservationVelocityAmount, Key: key, Amount: txn.Amount, WindowMin: r.cfg.WindowMin,
		})
		return nil
	}

	sum := r.counter.GetVelocityAmount(ctx, key, r.cfg.WindowMin)
	sum += txn.Amount
	if sum < r.cfg.MaxAmount {
		return nil
	}
	return &engine.Hit{
		RuleID: r.id, RuleName: r.name, Decision: r.verdict,
		Detail: fmt.Sprintf("velocity_amount %s=%s sum=%d in %dmin (limit %d)",
			r.cfg.ScopeBy, key, sum, r.cfg.WindowMin, r.cfg.MaxAmount),
	}
}

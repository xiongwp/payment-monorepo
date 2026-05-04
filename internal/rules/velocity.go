package rules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── velocity: N 笔 / T 分钟 速率限制 ──────────────────────────────
//
// 资损修复 P1-1：原 GetVelocity → 比较 + Report 时桶 +1 在并发下有 race。
// counter 实现 AtomicCounter 时改用 IncrIfBelowVelocity 原子预扣；不实现时
// 退回原行为。

type VelocityConfig struct {
	MaxCount   int    `json:"max_count"`
	WindowMin  int    `json:"window_min"`
	ScopeBy    string `json:"scope_by"`
	Method     string `json:"method"`
	DecisionOn string `json:"decision"`
}

type velocityRule struct {
	id, name string
	enabled  bool
	decision engine.Decision
	cfg      VelocityConfig
	counter  store.Counter
}

func VelocityFactory(counter store.Counter) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg VelocityConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		d := engine.Deny
		if cfg.DecisionOn == "REVIEW" {
			d = engine.Review
		}
		return &velocityRule{
			id: id, name: name, enabled: enabled,
			decision: d, cfg: cfg, counter: counter,
		}, nil
	}
}

func (r *velocityRule) ID() string    { return r.id }
func (r *velocityRule) Name() string  { return r.name }
func (r *velocityRule) Type() string  { return "velocity" }
func (r *velocityRule) Enabled() bool { return r.enabled }

func (r *velocityRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.cfg.Method != "" && txn.PaymentMethod != r.cfg.Method {
		return nil
	}
	key := scopeKey(r.cfg.ScopeBy, txn)
	ac, atomicOK := r.counter.(store.AtomicCounter)
	tracker := store.ReservationFromContext(ctx)
	if atomicOK && tracker != nil {
		newCount, ok, _ := ac.IncrIfBelowVelocity(ctx, key, r.cfg.WindowMin, r.cfg.MaxCount)
		if !ok {
			return &engine.Hit{
				RuleID: r.id, RuleName: r.name, Decision: r.decision,
				Detail: fmt.Sprintf("%d 笔 / %d 分钟内（限 %d 笔, atomic）", newCount, r.cfg.WindowMin, r.cfg.MaxCount),
			}
		}
		tracker.Add(store.Reservation{Kind: store.ReservationVelocity, Key: key, WindowMin: r.cfg.WindowMin})
		return nil
	}
	count := r.counter.GetVelocity(ctx, key, r.cfg.WindowMin)
	if count >= r.cfg.MaxCount {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: r.decision,
			Detail: fmt.Sprintf("%d 笔 / %d 分钟内（限 %d 笔）", count, r.cfg.WindowMin, r.cfg.MaxCount),
		}
	}
	return nil
}

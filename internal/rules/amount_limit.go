// Package rules 实现各种风控规则类型。
package rules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xiongwp/risk-manage/internal/engine"
	"github.com/xiongwp/risk-manage/internal/store"
)

// ─── amount_limit: 单笔 / 日 / 月限额 ──────────────────────────────
//
// 资损修复 P1-1：原 "Get → 比较 → Allow + Report 时 Incr" 在高并发下有
// TOCTOU race（拆单绕过限额）。改为：当 counter 实现 store.AtomicCounter
// 时走原子预扣路径（IncrIfBelowDaily / IncrIfBelowMonthly），规则 Evaluate
// 命中即"占位"，决策汇总后 service 层根据 Decision 做 commit / cancel。
// 不实现 AtomicCounter 时退回原 Get + 比较（向后兼容）。

type AmountLimitConfig struct {
	MaxPerTxn  int64  `json:"max_per_txn"`
	MaxDaily   int64  `json:"max_daily"`
	MaxMonthly int64  `json:"max_monthly"`
	ScopeBy    string `json:"scope_by"`
	Currency   string `json:"currency"`
	Method     string `json:"method"`
}

type amountLimitRule struct {
	id, name string
	enabled  bool
	decision engine.Decision
	cfg      AmountLimitConfig
	counter  store.Counter
}

func AmountLimitFactory(counter store.Counter) engine.RuleFactory {
	return func(id, name string, enabled bool, raw json.RawMessage) (engine.Rule, error) {
		var cfg AmountLimitConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
		return &amountLimitRule{
			id: id, name: name, enabled: enabled,
			decision: engine.Deny, cfg: cfg, counter: counter,
		}, nil
	}
}

func (r *amountLimitRule) ID() string    { return r.id }
func (r *amountLimitRule) Name() string  { return r.name }
func (r *amountLimitRule) Type() string  { return "amount_limit" }
func (r *amountLimitRule) Enabled() bool { return r.enabled }

func (r *amountLimitRule) Evaluate(ctx context.Context, txn *engine.TxnContext) *engine.Hit {
	if r.cfg.Currency != "" && txn.Currency != r.cfg.Currency {
		return nil
	}
	if r.cfg.Method != "" && txn.PaymentMethod != r.cfg.Method {
		return nil
	}

	if r.cfg.MaxPerTxn > 0 && txn.Amount > r.cfg.MaxPerTxn {
		return &engine.Hit{
			RuleID: r.id, RuleName: r.name, Decision: r.decision,
			Detail: fmt.Sprintf("单笔 %d > 限额 %d", txn.Amount, r.cfg.MaxPerTxn),
		}
	}

	key := scopeKey(r.cfg.ScopeBy, txn)
	ac, atomicOK := r.counter.(store.AtomicCounter)
	tracker := store.ReservationFromContext(ctx)

	if r.cfg.MaxDaily > 0 {
		if atomicOK && tracker != nil {
			newVal, ok, _ := ac.IncrIfBelowDaily(ctx, key, txn.Amount, r.cfg.MaxDaily, 0)
			if !ok {
				return &engine.Hit{
					RuleID: r.id, RuleName: r.name, Decision: r.decision,
					Detail: fmt.Sprintf("日累计 %d + 本笔 %d > 限额 %d (atomic)", newVal, txn.Amount, r.cfg.MaxDaily),
				}
			}
			tracker.Add(store.Reservation{Kind: store.ReservationDaily, Key: key, Amount: txn.Amount})
		} else {
			daily := r.counter.GetDaily(ctx, key)
			if daily+txn.Amount > r.cfg.MaxDaily {
				return &engine.Hit{
					RuleID: r.id, RuleName: r.name, Decision: r.decision,
					Detail: fmt.Sprintf("日累计 %d + 本笔 %d > 限额 %d", daily, txn.Amount, r.cfg.MaxDaily),
				}
			}
		}
	}

	if r.cfg.MaxMonthly > 0 {
		if atomicOK && tracker != nil {
			newVal, ok, _ := ac.IncrIfBelowMonthly(ctx, key, txn.Amount, r.cfg.MaxMonthly, 0)
			if !ok {
				return &engine.Hit{
					RuleID: r.id, RuleName: r.name, Decision: r.decision,
					Detail: fmt.Sprintf("月累计 %d + 本笔 %d > 限额 %d (atomic)", newVal, txn.Amount, r.cfg.MaxMonthly),
				}
			}
			tracker.Add(store.Reservation{Kind: store.ReservationMonthly, Key: key, Amount: txn.Amount})
		} else {
			monthly := r.counter.GetMonthly(ctx, key)
			if monthly+txn.Amount > r.cfg.MaxMonthly {
				return &engine.Hit{
					RuleID: r.id, RuleName: r.name, Decision: r.decision,
					Detail: fmt.Sprintf("月累计 %d + 本笔 %d > 限额 %d", monthly, txn.Amount, r.cfg.MaxMonthly),
				}
			}
		}
	}
	return nil
}

func scopeKey(scopeBy string, txn *engine.TxnContext) string {
	switch scopeBy {
	case "customer":
		return "customer:" + txn.CustomerID
	case "ip":
		return "ip:" + txn.IPAddress
	case "device":
		return "device:" + txn.DeviceID
	default:
		return "merchant:" + txn.MerchantID
	}
}

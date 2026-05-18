// refund.go — SP-9 收到 refund.completed 事件后, 按 graph.reversal.strategy 自动
// 把对应 transfer_group 下所有 Transfer 反向冲销 (Reversal), 可选同步 refund
// ApplicationFee.
//
// 触发链:
//
//	refund-engine.refund.completed (Kafka topic = recon.refund.events)
//	  ↓ subscriber → engine.HandleRefund
//	1. 查 charge_id 上一次 RunPlan (RunRepo.GetByCharge)
//	2. 拉 plan.TransferGroup 下所有 Transfer (TransferRepo.ListByGroup)
//	3. 按 graph.spec.reversal.strategy 算每条 Transfer 应 reverse 多少
//	4. 创建 Reversal + AddReversedAmount + (可选) AppFee.AddRefundedAmount
//	5. publish reversal.succeeded 事件
//
// Strategy:
//
//	"proportional" (默认): 按 refund_amount / charge_amount 比例平均退每条 transfer
//	"fixed_from_platform": 整笔从 platform 自营账户扣 (不触动卖家)
//	"fail_if_imbalance":   只有所有 transfer 都能完全 reverse 才执行,否则报错
//
// 幂等: 同 refund_id → 同 idempotency_key → 重复消费安全.
package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"

	"go.uber.org/zap"
)

// RefundEvent 来自 refund-engine 的退款完成事件.
type RefundEvent struct {
	RefundID      string `json:"refund_id"`
	ChargeID      string `json:"charge_id"`
	AmountMinor   int64  `json:"amount_minor"`   // 本次退款金额 (可 < charge 全额)
	Currency      string `json:"currency"`
	Reason        string `json:"reason,omitempty"` // duplicate / fraudulent / requested_by_customer
	TraceID       string `json:"trace_id,omitempty"`
	CompletedAt   time.Time `json:"completed_at"`
}

// ReversalExtRepo 给 SP-9 用 — workflow 不直接依赖 repo 包, 通过接口.
type ReversalExtRepo interface {
	Insert(ctx context.Context, rv *domain.Reversal) error
}

// ReversalApplier SP-AC-7 R1: 原子地把 (INSERT reversal + UPDATE transfer.reversed_amount) 包进一个 DB 事务.
// 实现见 repo.ApplyReversalAtomic; main.go 把 *sql.DB 闭包进来注入. nil → refund.go 退化到非事务写两步.
type ReversalApplier interface {
	Apply(ctx context.Context, rv *domain.Reversal, deltaReversed int64) error
}

// TransferReverseRepo SP-9 用: 拉 transfer_group + 累加 reversed_amount.
type TransferReverseRepo interface {
	ListByGroup(ctx context.Context, group string) ([]*domain.Transfer, error)
	AddReversedAmount(ctx context.Context, id string, delta int64) error
}

// AppFeeRefundRepo SP-9 用: 按 charge 拉 + 累加 refunded.
type AppFeeRefundRepo interface {
	ListByCharge(ctx context.Context, charge string) ([]*domain.ApplicationFee, error)
	AddRefundedAmount(ctx context.Context, id string, delta int64) error
}

// HandleRefund 处理 refund.completed 事件.
//
// 依赖 (engine 上挂的字段 + 额外两个 repo):
//   - e.RunRepo.GetByCharge   找原 RunPlan
//   - e.TransferRepo (扩展接口) ListByGroup + AddReversedAmount
//   - e.AppFeeRepo (扩展接口)   ListByCharge + AddRefundedAmount
//   - reversalRepo            Insert
//
// 失败处理: 单条 Reversal 失败 log error 继续 (其它 transfer 仍要反向);
// 全部失败 → 返回 error.
func (e *Engine) HandleRefund(
	ctx context.Context,
	ev RefundEvent,
	trRepo TransferReverseRepo,
	feeRepo AppFeeRefundRepo,
	rvRepo ReversalExtRepo,
) error {
	if ev.RefundID == "" || ev.ChargeID == "" {
		return errors.New("refund event: missing refund_id or charge_id")
	}
	if trRepo == nil || rvRepo == nil {
		return errors.New("HandleRefund requires TransferRepo + ReversalRepo")
	}

	// 1) 找原 RunPlan (取最近一次 charge 关联的)
	plans, err := e.RunRepo.GetByCharge(ctx, ev.ChargeID)
	if err != nil {
		return fmt.Errorf("get plans by charge: %w", err)
	}
	if len(plans) == 0 {
		e.Log.Warn("refund: no run plan for charge",
			zap.String("refund_id", ev.RefundID),
			zap.String("charge_id", ev.ChargeID))
		return nil // 不是错误, 可能此 charge 没走过资金流
	}
	// 多个 plan 取最新一个 (按 created_at desc, RunRepo 排序约定)
	plan := plans[0]
	if plan.TransferGroup == "" {
		e.Log.Info("refund: plan has no transfer_group (old plan), skipping reversal",
			zap.Int64("plan_id", plan.ID))
		return nil
	}

	// 2) 拉同 group 全部 transfer
	transfers, err := trRepo.ListByGroup(ctx, plan.TransferGroup)
	if err != nil {
		return fmt.Errorf("list transfers by group: %w", err)
	}
	if len(transfers) == 0 {
		return nil // 没东西可 reverse
	}

	// 3) Reversal strategy. 目前只实现 proportional (按比例退);
	// fixed_from_platform / fail_if_imbalance 是占位, 留 Phase 3 接.
	// Graph 反查暂略 (RunPlan.GraphID 拿到 → GraphRepo.GetByID 是另一个 trip), 默认走 proportional.

	// 4) 按 proportional 算每条 transfer reverse 多少
	totalCharge := int64(0)
	for _, t := range transfers {
		totalCharge += t.AmountMinor
	}
	if totalCharge == 0 {
		return errors.New("refund: transfer group total is 0")
	}
	// 退款比例 = refund_amount / charge_total
	// reverse_per_transfer = transfer.amount * ratio
	// (rounding: floor 给每条, 尾差归最后一条)
	allocs := make([]int64, len(transfers))
	allocated := int64(0)
	for i, t := range transfers {
		alloc := t.AmountMinor * ev.AmountMinor / totalCharge
		// 不能超 RemainingReversible
		if rem := t.RemainingReversible(); alloc > rem {
			alloc = rem
		}
		allocs[i] = alloc
		allocated += alloc
	}
	// 尾差补到最后一条非满的 transfer
	if diff := ev.AmountMinor - allocated; diff > 0 {
		for i := len(transfers) - 1; i >= 0; i-- {
			rem := transfers[i].RemainingReversible() - allocs[i]
			if rem > 0 {
				add := diff
				if add > rem {
					add = rem
				}
				allocs[i] += add
				diff -= add
				if diff == 0 {
					break
				}
			}
		}
	}

	// 5) 对每条 transfer 创建 Reversal + 累加 reversed
	reason := orDefaultStr(ev.Reason, domain.ReversalReasonRequestedByCustomer)
	now := time.Now().UTC()
	var firstErr error
	for i, t := range transfers {
		if allocs[i] <= 0 {
			continue
		}
		rv := &domain.Reversal{
			ID:             "tr_rev_" + randHex8(),
			Transfer:       t.ID,
			AmountMinor:    allocs[i],
			Currency:       t.Currency,
			Reason:         reason,
			Status:         domain.ReversalStatusPending,
			IdempotencyKey: ev.RefundID + "::" + t.ID,
			GraphRunID:     plan.ID,
			CreatedAt:      now,
		}
		// SP-AC-7 R1: 优先用 atomic applier (一个 sql.Tx 把 INSERT reversal + UPDATE transfer 包起来).
		// 退化路径 (applier nil) 保留, 但会 log warn 提醒资金一致性窗口.
		if e.ReversalApply != nil {
			rv.Status = domain.ReversalStatusSucceeded
			if err := e.ReversalApply.Apply(ctx, rv, allocs[i]); err != nil {
				e.Log.Error("ApplyReversalAtomic failed",
					zap.String("refund_id", ev.RefundID),
					zap.String("transfer_id", t.ID), zap.Error(err))
				// SP-AC-7 R5: 失败 → 排入 outbox 让 ReversalRetryWorker 后台重试.
				// 没接 retry queue 时退化为 firstErr 返回 (Kafka consumer 会重投).
				if e.ReversalRetry != nil {
					if qErr := e.ReversalRetry.Enqueue(ctx, rv.ID, t.ID, allocs[i], err); qErr != nil {
						e.Log.Error("ReversalRetry enqueue failed; falling back to first-err",
							zap.String("reversal_id", rv.ID), zap.Error(qErr))
						if firstErr == nil {
							firstErr = err
						}
					} else {
						e.Log.Info("reversal failure enqueued for retry",
							zap.String("reversal_id", rv.ID),
							zap.String("transfer_id", t.ID))
					}
				} else if firstErr == nil {
					firstErr = err
				}
				rv.Status = domain.ReversalStatusFailed
				rv.FailureMessage = err.Error()
				continue
			}
		} else {
			// 退化路径: 非事务两步 (有不一致窗口, dev/memory 模式).
			e.Log.Warn("ReversalApplier not wired; refund 走非事务两步路径, 资金不一致窗口存在",
				zap.String("transfer_id", t.ID))
			if err := rvRepo.Insert(ctx, rv); err != nil {
				e.Log.Error("reversal insert failed",
					zap.String("refund_id", ev.RefundID),
					zap.String("transfer_id", t.ID), zap.Error(err))
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := trRepo.AddReversedAmount(ctx, t.ID, allocs[i]); err != nil {
				e.Log.Error("AddReversedAmount failed",
					zap.String("transfer_id", t.ID), zap.Error(err))
				rv.Status = domain.ReversalStatusFailed
				rv.FailureMessage = err.Error()
				continue
			}
			rv.Status = domain.ReversalStatusSucceeded
		}
		e.publishEvent(ctx, EventReversalSucceeded, rv)
		// 同步 update transfer 状态事件
		if allocs[i] >= t.RemainingReversible() {
			e.publishEvent(ctx, EventTransferReversed, t)
		} else {
			e.publishEvent(ctx, EventTransferPartiallyReversed, t)
		}
	}

	// 6) (可选) 按比例退 ApplicationFee
	if feeRepo != nil {
		fees, err := feeRepo.ListByCharge(ctx, ev.ChargeID)
		if err == nil {
			for _, f := range fees {
				// 比例同 transfer: refund_amount / charge_total
				feeRefund := f.AmountMinor * ev.AmountMinor / totalCharge
				if rem := f.RemainingRefundable(); feeRefund > rem {
					feeRefund = rem
				}
				if feeRefund > 0 {
					_ = feeRepo.AddRefundedAmount(ctx, f.ID, feeRefund)
					f.RefundedAmount += feeRefund
					if f.RefundedAmount >= f.AmountMinor {
						e.publishEvent(ctx, EventAppFeeRefunded, f)
					} else {
						e.publishEvent(ctx, EventAppFeePartiallyRefunded, f)
					}
				}
			}
		}
	}
	return firstErr
}

// ReversalSpec strategy 字符串常量 — graph.spec.reversal.strategy 配置用.
//
// 当前只实现 proportional, 其它留占位.
const (
	ReversalSpecStrategyProportional      = "proportional"
	ReversalSpecStrategyFixedFromPlatform = "fixed_from_platform"
	ReversalSpecStrategyFailIfImbalance   = "fail_if_imbalance"
)

func randHex8() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

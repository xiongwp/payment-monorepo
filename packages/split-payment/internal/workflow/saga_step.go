// saga_step.go — SP-3A 把 RunPlan 翻译成持久化 saga step 链.
//
// Translator 输出 typed Transfer/AppFee/Payout 之后, BuildSteps() 把它们包成
// SagaStep, 每个 step:
//   - Execute: 调 accounting (post 一笔) + Repo.Insert 落表 + 状态置 posted/collected
//   - Compensate: Repo.AddReversedAmount / 反向 accounting batch (Transfer 才有)
//
// 顺序约定:
//   1. 先 ApplicationFee (平台抽成优先入账, 商户分账剩余的)
//   2. 然后所有 Transfer
//   3. 最后 Payout (一般是 hold 期满 / 手动触发, graph 内不直接产生)
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"reconcile-system/packages/split-payment/internal/domain"
)

// StepDeps 编排 saga step 时注入的依赖.
//
// SP-AC-7: 删除老 Accounting *clients.AccountingClient 字段 — saga step 当前实现
// (saga_step.go:121-122) 只翻状态不真调 accounting, 故 deps.Accounting 实际未使用.
type StepDeps struct {
	TransferRepo TransferRepo
	AppFeeRepo   AppFeeRepo
	PayoutRepo   PayoutRepo
	// Reversal 用 (Transfer 补偿时调)
	TransferReverse TransferReverseRepo
	ReversalInsert  ReversalExtRepo

	Events EventPublisher
}

// BuildSteps 把 RunPlan 编排成 saga step 序列.
//
// 顺序: AppFees → Transfers → Payouts (即 plan.AmplicationFees → plan.Transfers → plan.Payouts).
// 每 step 的 Execute / Compensate 闭包持 deps 引用.
func BuildSteps(plan *domain.RunPlan, deps StepDeps) []SagaStep {
	steps := []SagaStep{}

	// 1) ApplicationFees
	for i := range plan.ApplicationFees {
		f := plan.ApplicationFees[i]
		body, _ := json.Marshal(f)
		steps = append(steps, SagaStep{
			Name:       "app_fee_" + f.ID,
			Kind:       StepKindApplicationFee,
			Payload:    body,
			Status:     StepPending,
			TimeoutSec: 30,
			Execute:    buildAppFeeExecute(f, deps),
			// AppFee 默认不 compensate (退款时走独立 reversal 流, 见 refund.go)
			Compensate: nil,
		})
	}

	// 2) Transfers
	for i := range plan.Transfers {
		t := plan.Transfers[i]
		body, _ := json.Marshal(t)
		steps = append(steps, SagaStep{
			Name:       "transfer_" + t.ID,
			Kind:       StepKindTransfer,
			Payload:    body,
			Status:     StepPending,
			TimeoutSec: 30,
			Execute:    buildTransferExecute(t, deps),
			Compensate: buildTransferCompensate(t, deps),
		})
	}

	// 3) Payouts (一般 graph 内不直接产生, 走 cron; 但 edge.kind=payout 也支持)
	for i := range plan.Payouts {
		p := plan.Payouts[i]
		body, _ := json.Marshal(p)
		steps = append(steps, SagaStep{
			Name:       "payout_" + p.ID,
			Kind:       StepKindPayout,
			Payload:    body,
			Status:     StepPending,
			TimeoutSec: 30,
			Execute:    buildPayoutExecute(p, deps),
			Compensate: nil, // Payout 取消走独立流, 不在 saga 自动 compensate
		})
	}

	return steps
}

// ─── Execute / Compensate 工厂 ─────────────────────────────────────────

func buildAppFeeExecute(f domain.ApplicationFee, deps StepDeps) StepFn {
	return func(ctx context.Context) error {
		if deps.AppFeeRepo == nil {
			return fmt.Errorf("AppFeeRepo not configured")
		}
		f.Status = domain.AppFeeStatusCollected
		if err := deps.AppFeeRepo.Insert(ctx, &f); err != nil {
			return fmt.Errorf("appfee insert: %w", err)
		}
		if deps.Events != nil {
			_ = deps.Events.Publish(ctx, EventAppFeeCollected, f)
		}
		return nil
	}
}

func buildTransferExecute(t domain.Transfer, deps StepDeps) StepFn {
	return func(ctx context.Context) error {
		if deps.TransferRepo == nil {
			return fmt.Errorf("TransferRepo not configured")
		}
		// 先 Insert (status=created, idempotent via uk_idem)
		t.Status = domain.TransferStatusCreated
		if err := deps.TransferRepo.Insert(ctx, &t); err != nil {
			return fmt.Errorf("transfer insert: %w", err)
		}
		// 调 accounting (这里简化: accounting batch 已在 engine 之前批量提交了,
		// 这里只翻状态. 真实生产 saga 每 step 独立 accounting tx 见 Phase 3B.)
		now := time.Now().UTC()
		if err := deps.TransferRepo.UpdateStatus(ctx, t.ID, domain.TransferStatusPosted, now); err != nil {
			return fmt.Errorf("transfer status: %w", err)
		}
		t.Status = domain.TransferStatusPosted
		t.PostedAt = now
		if deps.Events != nil {
			_ = deps.Events.Publish(ctx, EventTransferPosted, t)
		}
		return nil
	}
}

func buildTransferCompensate(t domain.Transfer, deps StepDeps) StepFn {
	return func(ctx context.Context) error {
		if deps.TransferReverse == nil || deps.ReversalInsert == nil {
			return fmt.Errorf("Reversal repos not configured (cannot compensate)")
		}
		// 全额 reverse
		rv := &domain.Reversal{
			ID:             genID("tr_rev"),
			Transfer:       t.ID,
			AmountMinor:    t.AmountMinor,
			Currency:       t.Currency,
			Reason:         domain.ReversalReasonExpiredUncaptured,
			Status:         domain.ReversalStatusSucceeded,
			IdempotencyKey: "saga_compensate::" + t.ID,
			GraphRunID:     t.GraphRunID,
			CreatedAt:      time.Now().UTC(),
		}
		if err := deps.ReversalInsert.Insert(ctx, rv); err != nil {
			return fmt.Errorf("reversal insert: %w", err)
		}
		if err := deps.TransferReverse.AddReversedAmount(ctx, t.ID, t.AmountMinor); err != nil {
			return fmt.Errorf("transfer add reversed: %w", err)
		}
		if deps.Events != nil {
			_ = deps.Events.Publish(ctx, EventReversalSucceeded, rv)
			_ = deps.Events.Publish(ctx, EventTransferReversed, t)
		}
		return nil
	}
}

func buildPayoutExecute(p domain.Payout, deps StepDeps) StepFn {
	return func(ctx context.Context) error {
		if deps.PayoutRepo == nil {
			return fmt.Errorf("PayoutRepo not configured")
		}
		p.Status = domain.PayoutStatusPending
		if err := deps.PayoutRepo.Insert(ctx, &p); err != nil {
			return fmt.Errorf("payout insert: %w", err)
		}
		if deps.Events != nil {
			_ = deps.Events.Publish(ctx, EventPayoutCreated, p)
		}
		return nil
	}
}

// ─── StepFactory 实现 (重启 resume 用) ────────────────────────────────

// DefaultStepFactory 重启后从 (Kind, Payload) 重新组装闭包.
type DefaultStepFactory struct {
	Deps StepDeps
}

// NewDefaultStepFactory.
func NewDefaultStepFactory(deps StepDeps) *DefaultStepFactory {
	return &DefaultStepFactory{Deps: deps}
}

// BuildExecute.
func (f *DefaultStepFactory) BuildExecute(_ context.Context, kind StepKind, payload json.RawMessage) (StepFn, error) {
	switch kind {
	case StepKindApplicationFee:
		var fee domain.ApplicationFee
		if err := json.Unmarshal(payload, &fee); err != nil {
			return nil, err
		}
		return buildAppFeeExecute(fee, f.Deps), nil
	case StepKindTransfer:
		var t domain.Transfer
		if err := json.Unmarshal(payload, &t); err != nil {
			return nil, err
		}
		return buildTransferExecute(t, f.Deps), nil
	case StepKindPayout:
		var p domain.Payout
		if err := json.Unmarshal(payload, &p); err != nil {
			return nil, err
		}
		return buildPayoutExecute(p, f.Deps), nil
	}
	return nil, fmt.Errorf("unknown step kind: %s", kind)
}

// BuildCompensate.
func (f *DefaultStepFactory) BuildCompensate(_ context.Context, kind StepKind, payload json.RawMessage) (StepFn, error) {
	switch kind {
	case StepKindTransfer:
		var t domain.Transfer
		if err := json.Unmarshal(payload, &t); err != nil {
			return nil, err
		}
		return buildTransferCompensate(t, f.Deps), nil
	}
	// AppFee / Payout: 没自动 compensate
	return nil, nil
}

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Convergence Job — draining → frozen 自动推进
//
// 周期性扫描所有 draining instance，评估收敛指标：
//   1. balance = 0          （I4 守卫前置）
//   2. age >= drain_p99     （时间窗下限）
//   3. open_anchor_count = 0  （所有 anchor 已终态）
//   4. stuck_anchor_count = 0 （无 stuck anchor，否则必须先 quarantined）
//
// 全部满足 → 通过 InstanceStateMachine 推进到 frozen（带 CAS 守卫）。
//
// 【方向 B 收益】anchor 与 instance 同分片，单片查询即可，无 fan-out 跨 100 片。
//
// 失败模式：
//   - 单 instance 检查/推进失败 → 累计到 errors，不影响其他
//   - state machine guard 拒绝 → 重试到下次 Tick
//   - CAS 冲突（其他副本已推进）→ 视作成功（幂等）
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §7
// ============================================================================

// DrainingInstanceLister 仓储入口：拿到所有 draining instance。
type DrainingInstanceLister interface {
	// ListDraining 返回所有 lifecycle_phase=draining 的 instance（按 draining_started_at 升序）。
	ListDraining(ctx context.Context, limit int) ([]*model.Account, error)
}

// InstancePhasePromoter 把 instance 从一个 phase 推到另一个。
// 推进前会调用 InstanceStateMachine.CheckTransition 校验业务守卫。
type InstancePhasePromoter interface {
	// PromoteToFrozen draining → frozen 原子推进。
	// 调用方先调 StateMachine 校验守卫，再调本方法做 CAS 更新。
	// CAS 失败返回 ErrInstanceVersionConflict（视作幂等成功）。
	PromoteToFrozen(ctx context.Context, accountNo string, expectedVersion int64, frozenAt time.Time) error
}

// ConvergenceJob draining → frozen 推进。
type ConvergenceJob struct {
	lister    DrainingInstanceLister
	stateMach *InstanceStateMachine
	promoter  InstancePhasePromoter
	locks     LockManager // 复用 scheduler 的锁机制
	owner     string
	clock     func() time.Time
}

// NewConvergenceJob 构造。
func NewConvergenceJob(
	lister DrainingInstanceLister,
	stateMach *InstanceStateMachine,
	promoter InstancePhasePromoter,
	locks LockManager,
	owner string,
	clock func() time.Time,
) *ConvergenceJob {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if owner == "" {
		owner = "convergence-job-anonymous"
	}
	return &ConvergenceJob{
		lister:    lister,
		stateMach: stateMach,
		promoter:  promoter,
		locks:     locks,
		owner:     owner,
		clock:     clock,
	}
}

// ConvergenceResult 一次 Tick 汇总。
type ConvergenceResult struct {
	Scanned   int
	Converged int // 推进到 frozen
	NotReady  int // 收敛条件未满足
	Errors    []ConvergenceError
}

// ConvergenceError 单 instance 处理失败诊断。
type ConvergenceError struct {
	AccountNo string
	Err       error
}

// Tick 触发一次收敛检查 + 推进。
//
// 处理顺序：
//   1. ListDraining 获取所有 draining instance
//   2. 对每个 instance 拿 logical 锁（与 scheduler 同锁，防止与切换并发）
//   3. 调用 StateMachine.CheckTransition(draining, frozen) 检查全部守卫
//   4. 守卫通过 → PromoteToFrozen
//   5. 失败累计到 errors
func (j *ConvergenceJob) Tick(ctx context.Context) (*ConvergenceResult, error) {
	result := &ConvergenceResult{}
	instances, err := j.lister.ListDraining(ctx, 1000)
	if err != nil {
		return nil, fmt.Errorf("convergence: list draining: %w", err)
	}
	for _, inst := range instances {
		result.Scanned++
		if err := j.processOne(ctx, inst, result); err != nil {
			result.Errors = append(result.Errors, ConvergenceError{
				AccountNo: inst.AccountNo,
				Err:       err,
			})
		}
	}
	return result, nil
}

func (j *ConvergenceJob) processOne(
	ctx context.Context, inst *model.Account, result *ConvergenceResult,
) error {
	if inst.LogicalAccountID == nil {
		return errors.New("draining instance missing logical_account_id")
	}

	// 加锁（与 scheduler 锁同一资源 — 防止与 PromoteAndDrain 并发）
	release, err := j.locks.AcquireForLogical(ctx, *inst.LogicalAccountID, j.owner)
	if err != nil {
		// 锁竞争 — 不算错误，等下一次 Tick
		return nil //nolint:nilerr // intentional
	}
	defer release()

	// State machine 校验所有守卫
	guard, err := j.stateMach.CheckTransition(ctx, inst, model.LifecyclePhaseFrozen)
	if err != nil {
		return fmt.Errorf("guard check: %w", err)
	}
	if !guard.Allowed {
		// 还不到推进的时候，记录但不报错
		result.NotReady++
		return nil
	}

	// 守卫通过 → 推进
	now := j.clock()
	if err := j.promoter.PromoteToFrozen(ctx, inst.AccountNo, inst.Version, now); err != nil {
		if errors.Is(err, ErrInstanceVersionConflict) {
			// 已被其他副本推进 → 幂等成功
			result.Converged++
			return nil
		}
		return fmt.Errorf("promote to frozen: %w", err)
	}
	result.Converged++
	return nil
}

// ErrInstanceVersionConflict CAS on instance.version 失败。
var ErrInstanceVersionConflict = errors.New("instance version conflict (CAS failed)")

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Instance Lifecycle State Machine — 服务层（业务守卫 + CAS 推进）
//
// 与 model.CanTransitionPhase 的关系：
//   - model 层只检查"图上是否有边"（纯函数，无 IO）
//   - service 层在边的基础上施加"业务守卫"（需要查 repo / 时钟）
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §4 / §7 / §10
// ============================================================================

// PhaseGuardResult 守卫结果。Allowed=false 时 Reason 给出原因，供运维/告警 trace 用。
type PhaseGuardResult struct {
	Allowed bool
	Reason  string
}

// AccountReaderForPhase 状态机依赖：能读账户 + 余额。
// 单独定义接口而非直接用 repo.AccountRepository，便于单测注入 fake。
type AccountReaderForPhase interface {
	// CountActiveByLogical 统计同一 logical_account 下 phase=active 的 instance 数。
	// 由 invariant I1 守卫使用。
	CountActiveByLogical(ctx context.Context, logicalAccountID int64) (int, error)

	// GetByAccountNo 取一行 account（用于复检 balance / phase）。
	GetByAccountNo(ctx context.Context, accountNo string) (*model.Account, error)
}

// AnchorReaderForPhase 状态机依赖：anchor 计数。
type AnchorReaderForPhase interface {
	CountOpenByAccountNo(ctx context.Context, accountNo string, globalTableIndex int) (int64, error)
	CountStuckByAccountNo(ctx context.Context, accountNo string, globalTableIndex int) (int64, error)
	OldestOpenAnchoredAt(ctx context.Context, accountNo string, globalTableIndex int) (*time.Time, error)
}

// PolicyReader 取策略（drain_p99, archive_grace 等）。
type PolicyReader interface {
	GetPolicy(ctx context.Context, logicalAccountID int64) (*model.LogicalAccountRotationPolicy, error)
}

// AnchorShardRouter 由 router 提供：给 instance 上属于哪几片 anchor。
// 这是一个抽象点——instance 上的 anchor 实际分布在多个 anchor shard 上（因为
// anchor shard 按 flow_id 哈希，与 account_no 无关）。守卫必须扫所有
// 100 个分片 SUM(open_count) 才能判定 instance 是否真的"无 open anchor"。
//
// 为简化测试，这里抽象成"按 instance 列举所有要扫的分片 id"。
type AnchorShardRouter interface {
	// AllAnchorShards 返回全部 globalTableIndex 列表（0..99）。
	AllAnchorShards() []int
}

// InstanceStateMachine instance phase 转换的业务守卫 + 推进。
type InstanceStateMachine struct {
	accounts      AccountReaderForPhase
	anchors       AnchorReaderForPhase
	policies      PolicyReader
	anchorShards  AnchorShardRouter
	clock         func() time.Time
}

// NewInstanceStateMachine 构造。clock 可注入；nil 时用 time.Now()。
func NewInstanceStateMachine(
	accounts AccountReaderForPhase,
	anchors AnchorReaderForPhase,
	policies PolicyReader,
	anchorShards AnchorShardRouter,
	clock func() time.Time,
) *InstanceStateMachine {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &InstanceStateMachine{
		accounts:     accounts,
		anchors:      anchors,
		policies:     policies,
		anchorShards: anchorShards,
		clock:        clock,
	}
}

// CheckTransition 纯检查：是否允许 from→to 转换。不修改任何状态。
//
// 检查顺序：
//   1. 图合法性（model.CanTransitionPhase）
//   2. 业务守卫（按目标 phase 不同应用不同规则）
//
// 返回 (result, err)：err 仅用于 repo IO 失败；不可达的转换由 result.Allowed=false 返回。
func (sm *InstanceStateMachine) CheckTransition(
	ctx context.Context, acc *model.Account, target model.LifecyclePhase,
) (PhaseGuardResult, error) {
	if acc == nil {
		return PhaseGuardResult{}, errors.New("InstanceStateMachine: nil account")
	}
	// Step 1: 图合法性
	if !model.CanTransitionPhase(acc.LifecyclePhase, target) {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("illegal transition %s -> %s", acc.LifecyclePhase, target),
		}, nil
	}
	// 没有 LogicalAccountID 的 instance 只能进 quarantined（兜底）
	if acc.LogicalAccountID == nil && target != model.LifecyclePhaseQuarantined {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  "instance has no logical_account_id; only quarantined transition allowed",
		}, nil
	}

	// Step 2: 业务守卫
	switch target {
	case model.LifecyclePhaseActive:
		return sm.guardEnterActive(ctx, acc)
	case model.LifecyclePhaseDraining:
		return sm.guardEnterDraining(ctx, acc)
	case model.LifecyclePhaseFrozen:
		return sm.guardEnterFrozen(ctx, acc)
	case model.LifecyclePhaseArchived:
		return sm.guardEnterArchived(ctx, acc)
	case model.LifecyclePhaseQuarantined:
		// quarantined 不施加守卫——任何时候运维都能拉过去隔离。
		return PhaseGuardResult{Allowed: true}, nil
	default:
		return PhaseGuardResult{Allowed: true}, nil
	}
}

// guardEnterActive provisioned → active：
//   - I1 不变量：同一 logical 下 active 计数必须为 0（否则会出现"双 active"灾难）
func (sm *InstanceStateMachine) guardEnterActive(
	ctx context.Context, acc *model.Account,
) (PhaseGuardResult, error) {
	if acc.LogicalAccountID == nil {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  "enter active: no logical_account_id",
		}, nil
	}
	cnt, err := sm.accounts.CountActiveByLogical(ctx, *acc.LogicalAccountID)
	if err != nil {
		return PhaseGuardResult{}, fmt.Errorf("guardEnterActive: count active: %w", err)
	}
	if cnt != 0 {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("I1 violation: %d existing active instance(s) under logical=%d", cnt, *acc.LogicalAccountID),
		}, nil
	}
	// PeriodStart 必须 ≤ now（不能让"未来"的 instance 立即激活）
	if acc.PeriodStart != nil && acc.PeriodStart.After(sm.clock()) {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("enter active too early: period_start=%s now=%s",
				acc.PeriodStart.Format(time.RFC3339), sm.clock().Format(time.RFC3339)),
		}, nil
	}
	return PhaseGuardResult{Allowed: true}, nil
}

// guardEnterDraining active → draining：
//   - 新 active 必须已就位（除非本 instance 是被运维显式 drain）
//
// 由 scheduler 在原子切换事务里调用：调用前已经 promote 了新 instance。
// 这里保留检查作为兜底，但宽松——若 acc 是当前 active 且 PeriodEnd 已过，
// 允许通过；具体的"新 active 已就位"由 scheduler 在事务内验证。
func (sm *InstanceStateMachine) guardEnterDraining(
	ctx context.Context, acc *model.Account,
) (PhaseGuardResult, error) {
	if acc.PeriodEnd == nil {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  "enter draining: missing period_end",
		}, nil
	}
	// 允许早到约 1 分钟（时钟漂移容忍）
	if sm.clock().Add(time.Minute).Before(*acc.PeriodEnd) {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("enter draining too early: period_end=%s now=%s",
				acc.PeriodEnd.Format(time.RFC3339), sm.clock().Format(time.RFC3339)),
		}, nil
	}
	return PhaseGuardResult{Allowed: true}, nil
}

// guardEnterFrozen draining → frozen：满足 §7.1 全部收敛指标
//   - open_anchor_count = 0  （所有分片求和）
//   - stuck_anchor_count = 0
//   - age >= drain_p99
//   - balance = 0
func (sm *InstanceStateMachine) guardEnterFrozen(
	ctx context.Context, acc *model.Account,
) (PhaseGuardResult, error) {
	if acc.LogicalAccountID == nil {
		return PhaseGuardResult{
			Allowed: false, Reason: "enter frozen: no logical_account_id",
		}, nil
	}

	// 1. balance 必须为 0（I4 守卫）
	if acc.Balance != 0 {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("enter frozen: balance=%d not zero", acc.Balance),
		}, nil
	}

	// 2. age >= drain_p99
	policy, err := sm.policies.GetPolicy(ctx, *acc.LogicalAccountID)
	if err != nil {
		return PhaseGuardResult{}, fmt.Errorf("guardEnterFrozen: get policy: %w", err)
	}
	if policy == nil {
		return PhaseGuardResult{
			Allowed: false, Reason: "enter frozen: no rotation policy configured",
		}, nil
	}
	if acc.DrainingStartedAt == nil {
		return PhaseGuardResult{
			Allowed: false, Reason: "enter frozen: missing draining_started_at",
		}, nil
	}
	requiredAge := time.Duration(policy.DrainP99Seconds) * time.Second
	age := sm.clock().Sub(*acc.DrainingStartedAt)
	if age < requiredAge {
		return PhaseGuardResult{
			Allowed: false,
			Reason: fmt.Sprintf("enter frozen: drain age=%s < drain_p99=%s",
				age.Truncate(time.Second), requiredAge.Truncate(time.Second)),
		}, nil
	}

	// 3. anchors: open=0 且 stuck=0（跨所有 anchor 分片）
	openTotal, stuckTotal, err := sm.sumAnchorCounters(ctx, acc.AccountNo)
	if err != nil {
		return PhaseGuardResult{}, err
	}
	if stuckTotal > 0 {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("enter frozen blocked: %d stuck anchor(s); must quarantine first", stuckTotal),
		}, nil
	}
	if openTotal > 0 {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("enter frozen: %d open anchor(s) remain", openTotal),
		}, nil
	}
	return PhaseGuardResult{Allowed: true}, nil
}

// guardEnterArchived frozen → archived：
//   - now > frozen_at + archive_grace
//   - balance = 0（再次确认 I4，防止 frozen 期间被错误调整）
func (sm *InstanceStateMachine) guardEnterArchived(
	ctx context.Context, acc *model.Account,
) (PhaseGuardResult, error) {
	if acc.LogicalAccountID == nil {
		return PhaseGuardResult{
			Allowed: false, Reason: "enter archived: no logical_account_id",
		}, nil
	}
	if acc.Balance != 0 {
		return PhaseGuardResult{
			Allowed: false,
			Reason:  fmt.Sprintf("enter archived: balance=%d not zero (I4 violation)", acc.Balance),
		}, nil
	}
	if acc.FrozenAt == nil {
		return PhaseGuardResult{
			Allowed: false, Reason: "enter archived: missing frozen_at",
		}, nil
	}
	policy, err := sm.policies.GetPolicy(ctx, *acc.LogicalAccountID)
	if err != nil {
		return PhaseGuardResult{}, fmt.Errorf("guardEnterArchived: get policy: %w", err)
	}
	if policy == nil {
		return PhaseGuardResult{
			Allowed: false, Reason: "enter archived: no rotation policy configured",
		}, nil
	}
	graceSecs := policy.ArchiveGraceSecs
	if graceSecs < 0 {
		graceSecs = 0
	}
	requiredAt := acc.FrozenAt.Add(time.Duration(graceSecs) * time.Second)
	if sm.clock().Before(requiredAt) {
		return PhaseGuardResult{
			Allowed: false,
			Reason: fmt.Sprintf("enter archived too early: now=%s required>=%s",
				sm.clock().Format(time.RFC3339), requiredAt.Format(time.RFC3339)),
		}, nil
	}
	return PhaseGuardResult{Allowed: true}, nil
}

// sumAnchorCounters 跨所有 anchor 分片求和 (open, stuck)。
// 任一分片读失败即返回错误——不能用部分数据做收敛决策。
func (sm *InstanceStateMachine) sumAnchorCounters(
	ctx context.Context, accountNo string,
) (open, stuck int64, err error) {
	shards := sm.anchorShards.AllAnchorShards()
	for _, gtbl := range shards {
		o, e := sm.anchors.CountOpenByAccountNo(ctx, accountNo, gtbl)
		if e != nil {
			return 0, 0, fmt.Errorf("sumAnchorCounters: shard %d: %w", gtbl, e)
		}
		s, e := sm.anchors.CountStuckByAccountNo(ctx, accountNo, gtbl)
		if e != nil {
			return 0, 0, fmt.Errorf("sumAnchorCounters: shard %d: %w", gtbl, e)
		}
		open += o
		stuck += s
	}
	return open, stuck, nil
}

// ============================================================================
// Anchor Status State Machine — 服务层
// ============================================================================

// AnchorTransitionResult 守卫结果。
type AnchorTransitionResult struct {
	Allowed bool
	Reason  string
}

// AnchorStateMachine anchor.status 转换的业务守卫。
type AnchorStateMachine struct{}

// NewAnchorStateMachine 构造。anchor 状态机当前是纯函数（无 IO 依赖）—— 业务守卫
// 已经包含在 model.CanTransitionAnchor 中。但保留 service-level wrapper 以便
// 后续添加业务规则（例如 forced settle 需要外部确认）。
func NewAnchorStateMachine() *AnchorStateMachine {
	return &AnchorStateMachine{}
}

// CheckTransition 是否允许 from→to。
//
// 额外守卫（图合法性之外）：
//   - active→migrated 要求调用方提供 target_account_no（在 service 层施加，repo 已经校验）
//   - trying→active 要求至少有一笔 transaction status=success；这里抽象为"上游决定调用"
func (sm *AnchorStateMachine) CheckTransition(
	from, to model.AnchorStatus,
) AnchorTransitionResult {
	if !model.CanTransitionAnchor(from, to) {
		return AnchorTransitionResult{
			Allowed: false,
			Reason:  fmt.Sprintf("illegal anchor transition %s -> %s", from, to),
		}
	}
	return AnchorTransitionResult{Allowed: true}
}

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrTransitionGuardRejected 业务守卫拒绝；result.Reason 给出详细原因。
	ErrTransitionGuardRejected = errors.New("state machine guard rejected")
)

// WrapGuardError 把 GuardResult 转成 error。Allowed=true 时返 nil；
// 便于调用方用 if err != nil 风格处理。
func WrapGuardError(r PhaseGuardResult) error {
	if r.Allowed {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrTransitionGuardRejected, r.Reason)
}

// WrapAnchorGuardError 同上，anchor 版本。
func WrapAnchorGuardError(r AnchorTransitionResult) error {
	if r.Allowed {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrTransitionGuardRejected, r.Reason)
}

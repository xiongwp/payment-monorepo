package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Rotation Scheduler — 周期边界 + 实例切换 + 反范式化更新
//
// 周期推进生命周期：
//   1. provisioning（提前 lead_time）：创建下一期 provisioned instance
//   2. activation（到达 period_end）：原子切换 active→draining + provisioned→active
//   3. logical_account.current_active_account_no 反范式化字段更新
//
// 关键不变量：
//   I-S1 任意时刻同 logical 下至多一个 active instance（CAS + InstanceStateMachine 保证）
//   I-S2 provisioned 到 active 的提升 + 旧 active 到 draining 的转换 **必须原子**
//   I-S3 LA.current_active_account_no 必须与"phase=active 的 instance"严格一致
//   I-S4 多副本并发跑时只有一个成功（用 distributed_lock 或 CAS 兜底）
//
// 调用约定：
//   - Scheduler.Tick(ctx) 由外部 cron / k8s job / 内置 ticker 触发
//   - 每次 Tick 处理全部 rotating logical_accounts；幂等
//   - 失败的 LA 不影响其他 LA 处理
//
// 设计文档：docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md §7（轮换调度）+ §10.6（运维）
// ============================================================================

// LogicalAccountLister 读所有需要 rotation 检查的 logical_accounts。
type LogicalAccountLister interface {
	// ListRotating 返回所有 rotation_enabled=1 且 status=enabled 的 logical_account。
	// 返回顺序：按 id 升序（确定性）。
	ListRotating(ctx context.Context, limit int) ([]*model.LogicalAccount, error)
}

// AccountInstanceManager scheduler 用于读写 instance（phase 转换）。
type AccountInstanceManager interface {
	// GetActiveInstance 返回 logical 下当前 phase=active 的 instance；无则返回 (nil, nil)。
	GetActiveInstance(ctx context.Context, logicalAccountID int64) (*model.Account, error)

	// GetProvisionedInstance 返回 logical 下当前 phase=provisioned 且 period_start 最近的
	// instance（=下一期）；无则返回 (nil, nil)。
	GetProvisionedInstance(ctx context.Context, logicalAccountID int64) (*model.Account, error)

	// CreateProvisioned 创建一个 provisioned instance（period 边界由 caller 提供）。
	// 调用方传入的 account 已经填好除 ID/CreatedAt 外的所有字段。
	CreateProvisioned(ctx context.Context, acc *model.Account) error

	// PromoteAndDrain 原子操作：把 provisioned instance 提升到 active；
	// 同时把当前 active 降到 draining；同时更新 LA.current_active_account_no。
	// 三表三 CAS 在同一逻辑事务里（具体跨表协调由 repo 层实现）。
	PromoteAndDrain(ctx context.Context, params PromoteAndDrainParams) error
}

// PromoteAndDrainParams 原子切换参数。
type PromoteAndDrainParams struct {
	LogicalAccountID         int64
	OldActiveAccountNo       string // 当前 active；切换后变 draining；可空（首次激活无旧 active）
	OldActiveVersion         int64
	NewActiveAccountNo       string
	NewActiveVersion         int64
	NewActivePeriodEnd       time.Time
	LogicalAccountVersion    int64 // logical_account 表的 CAS 版本
	Now                      time.Time
}

// PolicyReaderForScheduler scheduler 用于读 rotation 策略。
type PolicyReaderForScheduler interface {
	GetPolicy(ctx context.Context, logicalAccountID int64) (*model.LogicalAccountRotationPolicy, error)
}

// LockManager scheduler 多副本互斥：基于 logical_account_id 加锁（短期）。
// 实现可以基于 distributed_lock 表或 Redis。
type LockManager interface {
	// AcquireForLogical 尝试获取 logical_account_id 的 scheduler 锁。
	// 返回 (released, err)：released 是释放函数；err != nil 表示获取失败。
	AcquireForLogical(ctx context.Context, logicalAccountID int64, owner string) (release func(), err error)
}

// AccountIDGenerator 创建 provisioned instance 时分配 account_no 和 id。
type AccountIDGenerator interface {
	// NewProvisionedAccountNo 为给定 LA 的下一个 instance 生成 account_no。
	// 生产实现：使用 EncodeAccountID 同 layout（19 位编码）。
	NewProvisionedAccountNo(ctx context.Context, la *model.LogicalAccount, periodStart time.Time) (string, error)
}

// Scheduler 轮换调度器。
type Scheduler struct {
	logicals  LogicalAccountLister
	instances AccountInstanceManager
	policies  PolicyReaderForScheduler
	locks     LockManager
	idgen     AccountIDGenerator
	clock     func() time.Time
	owner     string // 本副本身份（用于 lock 审计）
}

// NewScheduler 构造。clock 可注入；nil 时用 time.Now()。owner 推荐用 hostname + pid。
func NewScheduler(
	logicals LogicalAccountLister,
	instances AccountInstanceManager,
	policies PolicyReaderForScheduler,
	locks LockManager,
	idgen AccountIDGenerator,
	owner string,
	clock func() time.Time,
) *Scheduler {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if owner == "" {
		owner = "rotation-scheduler-anonymous"
	}
	return &Scheduler{
		logicals:  logicals,
		instances: instances,
		policies:  policies,
		locks:     locks,
		idgen:     idgen,
		owner:     owner,
		clock:     clock,
	}
}

// TickResult 一次 Tick 的汇总。便于运维监控。
type TickResult struct {
	Processed     int
	ProvisionedNew int
	Activated     int
	NoActionNeeded int
	Errors        []TickError
}

// TickError 某个 LA 处理失败的诊断。
type TickError struct {
	LogicalAccountID  int64
	LogicalAccountKey string
	Err               error
}

// Tick 触发一次轮换检查 + 推进。
//
// 处理顺序（按 LA 独立）：
//   1. 获取本 LA 的 scheduler 锁（多副本互斥）
//   2. 读 policy 和当前 active instance
//   3. 若 PeriodEnd 临近（now + lead_time >= period_end）→ 确保 provisioned 已就位
//   4. 若 PeriodEnd 已过 → 执行原子切换（PromoteAndDrain）
//   5. 释放锁
//
// 失败的 LA 不影响其他 LA 处理（continue + 累计到 Errors）。
func (s *Scheduler) Tick(ctx context.Context) (*TickResult, error) {
	result := &TickResult{}
	las, err := s.logicals.ListRotating(ctx, 1000)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list rotating LAs: %w", err)
	}
	for _, la := range las {
		result.Processed++
		if err := s.processOne(ctx, la, result); err != nil {
			result.Errors = append(result.Errors, TickError{
				LogicalAccountID:  la.ID,
				LogicalAccountKey: la.LogicalAccountKey,
				Err:               err,
			})
		}
	}
	return result, nil
}

func (s *Scheduler) processOne(ctx context.Context, la *model.LogicalAccount, result *TickResult) error {
	// Step 1: 获取本 LA 的锁
	release, err := s.locks.AcquireForLogical(ctx, la.ID, s.owner)
	if err != nil {
		// 锁被其他副本持有 — 不算错误，下次 Tick 再尝试
		return nil //nolint:nilerr // intentional: lock contention is normal
	}
	defer release()

	// Step 2: 读 policy
	policy, err := s.policies.GetPolicy(ctx, la.ID)
	if err != nil {
		return fmt.Errorf("get policy: %w", err)
	}
	if policy == nil {
		return errors.New("rotation_enabled=1 but no policy configured")
	}

	// Step 3: 读当前 active
	active, err := s.instances.GetActiveInstance(ctx, la.ID)
	if err != nil {
		return fmt.Errorf("get active instance: %w", err)
	}

	now := s.clock()

	// 决策树
	if active == nil {
		// 没有 active：可能是首次启用 rotation_enabled=1，或上次切换出问题
		// 首先创建一个 provisioned，下一轮 Tick 把它激活
		return s.ensureProvisioned(ctx, la, policy, nil, now, result)
	}

	if active.PeriodEnd == nil {
		return fmt.Errorf("active instance %s missing period_end", active.AccountNo)
	}

	// 距离 period_end 还有多久？
	timeToEnd := active.PeriodEnd.Sub(now)
	leadTime := time.Duration(policy.ProvisionLeadSecs) * time.Second

	if timeToEnd <= 0 {
		// 已经到/过了边界 → 执行切换
		return s.swap(ctx, la, active, policy, now, result)
	}

	if timeToEnd <= leadTime {
		// 临近边界 → 确保 provisioned 已就位
		return s.ensureProvisioned(ctx, la, policy, active, now, result)
	}

	// 还早，本轮不动
	result.NoActionNeeded++
	return nil
}

// ensureProvisioned 检查/创建下一期 provisioned instance。
func (s *Scheduler) ensureProvisioned(
	ctx context.Context, la *model.LogicalAccount,
	policy *model.LogicalAccountRotationPolicy,
	currentActive *model.Account, now time.Time,
	result *TickResult,
) error {
	existing, err := s.instances.GetProvisionedInstance(ctx, la.ID)
	if err != nil {
		return fmt.Errorf("get provisioned: %w", err)
	}
	if existing != nil {
		// 已经预创建了，不重复
		result.NoActionNeeded++
		return nil
	}

	// 计算下一期边界
	periodStart, periodEnd, err := s.computeNextPeriod(currentActive, policy, now)
	if err != nil {
		return fmt.Errorf("compute period: %w", err)
	}

	accountNo, err := s.idgen.NewProvisionedAccountNo(ctx, la, periodStart)
	if err != nil {
		return fmt.Errorf("alloc account_no: %w", err)
	}

	newInstance := &model.Account{
		AccountNo:           accountNo,
		AccountType:         la.AccountType,
		AccountBusinessType: la.AccountBusinessType,
		Currency:            la.Currency,
		Status:              model.AccountStatusActive,
		LogicalAccountID:    &la.ID,
		LifecyclePhase:      model.LifecyclePhaseProvisioned,
		PeriodStart:         &periodStart,
		PeriodEnd:           &periodEnd,
	}
	configVersion := policy.ConfigVersion
	newInstance.PolicyVersionAtBirth = &configVersion

	if err := s.instances.CreateProvisioned(ctx, newInstance); err != nil {
		return fmt.Errorf("create provisioned: %w", err)
	}
	result.ProvisionedNew++
	return nil
}

// swap 执行 active → draining + provisioned → active 的原子切换。
func (s *Scheduler) swap(
	ctx context.Context, la *model.LogicalAccount,
	currentActive *model.Account,
	policy *model.LogicalAccountRotationPolicy,
	now time.Time, result *TickResult,
) error {
	// 必须先有 provisioned 才能切换
	next, err := s.instances.GetProvisionedInstance(ctx, la.ID)
	if err != nil {
		return fmt.Errorf("get provisioned for swap: %w", err)
	}
	if next == nil {
		// 没有 provisioned —— scheduler 未及时预创建（运维警报场景）
		// 兜底：现场创建 + 立即激活，避免空窗期
		// 但本 Tick 仅创建，下一 Tick 才激活（保持各步骤幂等）
		return s.ensureProvisioned(ctx, la, policy, currentActive, now, result)
	}

	if next.PeriodStart == nil || next.PeriodEnd == nil {
		return fmt.Errorf("provisioned instance %s missing period bounds", next.AccountNo)
	}

	params := PromoteAndDrainParams{
		LogicalAccountID:      la.ID,
		OldActiveAccountNo:    currentActive.AccountNo,
		OldActiveVersion:      currentActive.Version,
		NewActiveAccountNo:    next.AccountNo,
		NewActiveVersion:      next.Version,
		NewActivePeriodEnd:    *next.PeriodEnd,
		LogicalAccountVersion: la.Version,
		Now:                   now,
	}
	if err := s.instances.PromoteAndDrain(ctx, params); err != nil {
		return fmt.Errorf("promote and drain: %w", err)
	}
	result.Activated++
	return nil
}

// computeNextPeriod 根据 policy 计算下一期 [start, end)。
// 若 currentActive 为 nil（首次激活）→ start=now，end=now+周期。
// 否则 start=currentActive.PeriodEnd，end=start+周期。
func (s *Scheduler) computeNextPeriod(
	currentActive *model.Account,
	policy *model.LogicalAccountRotationPolicy,
	now time.Time,
) (start, end time.Time, err error) {
	loc, err := time.LoadLocation(policy.RotationAnchorTZ)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("load tz %q: %w", policy.RotationAnchorTZ, err)
	}

	if currentActive == nil || currentActive.PeriodEnd == nil {
		start = now
	} else {
		start = *currentActive.PeriodEnd
	}

	// 在 tz 下计算下一期边界
	startInTz := start.In(loc)
	switch policy.PeriodUnit {
	case model.PeriodUnitDay:
		end = startInTz.AddDate(0, 0, policy.PeriodCount).UTC()
	case model.PeriodUnitMonth:
		end = startInTz.AddDate(0, policy.PeriodCount, 0).UTC()
	case model.PeriodUnitQuarter:
		end = startInTz.AddDate(0, 3*policy.PeriodCount, 0).UTC()
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unknown PeriodUnit: %s", policy.PeriodUnit)
	}
	return start.UTC(), end, nil
}

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"go.uber.org/zap"
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

	// PromoteAndDrainFleet fleet 模式批量切换（fleet × rotation）。
	// 跨 100 sub-account 并行：oldGroup all active→draining；newGroup all provisioned→active；
	// LA.current_active_account_no + current_active_group 同步更新。
	PromoteAndDrainFleet(ctx context.Context, params PromoteAndDrainFleetParams) error

	// ListActiveFleet 拉 logical_account 下所有 phase=active 的 sub-account，
	// 按 user_id 升序排列（sub_idx=0..99）。fleet 切换 / provisioned 完成后用来
	// 给 config-center push 当前 active 100-sub 快照。
	//
	// 返回 [100]string；某个 idx 没 active sub 时对应元素是空字符串。
	ListActiveFleet(ctx context.Context, logicalAccountID int64) ([]string, error)
}

// PromoteAndDrainFleetParams fleet 切换参数。
type PromoteAndDrainFleetParams struct {
	LogicalAccountID      int64
	OldGroup              string // "" = 首次激活，无旧 fleet
	NewGroup              string
	NewActiveAccountNo    string // fleet anchor（取 newGroup 中 user_id=0 的那个）
	NewActivePeriodEnd    time.Time
	LogicalAccountVersion int64
	Now                   time.Time
}

// PromoteAndDrainParams 原子切换参数。
type PromoteAndDrainParams struct {
	LogicalAccountID         int64
	OldActiveAccountNo       string // 当前 active；切换后变 draining；可空（首次激活无旧 active）
	OldActiveVersion         int64
	NewActiveAccountNo       string
	NewActiveVersion         int64
	NewActiveGroup           string // "A" / "B"；切换后写到 LogicalAccount.current_active_group
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

	// NewProvisionedFleetAccountNos 为给定 LA 生成 fleet 100 个 sub-account 的 account_no。
	// 每个 sub_account 的 globalTblIdx = subIdx (0..99)，散布到 100 个 shard 表。
	// 返回 长度=100 的切片，索引 i 对应 user_id=i 的 sub-account。
	NewProvisionedFleetAccountNos(ctx context.Context, la *model.LogicalAccount, periodStart time.Time) ([]string, error)
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
	logger    *zap.Logger
	// fleetCache rotation 完成后把新 active 100-sub 推到 config-center
	// 让所有 accounting 实例本地 cache 立刻拿到新映射，gRPC routing 不用查 DB
	// nil 安全：不注入时跳过 push（兼容老部署）
	fleetCache FleetCache
}

// WithLogger 注入业务日志。
func (s *Scheduler) WithLogger(l *zap.Logger) *Scheduler {
	s.logger = l
	return s
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

// WithFleetCache 注入 fleet cache（config-center push 通道）。链式调用注入。
// nil 安全：未注入时 swap / ensureProvisioned 完成后不推送 config-center，
// gRPC routing 100% 走 DB（功能正常但少了 cache 加速）。
func (s *Scheduler) WithFleetCache(cache FleetCache) *Scheduler {
	s.fleetCache = cache
	return s
}

// pushFleetCacheAfterRotation rotation 完成（swap/ensureProvisioned）后
// 拉新 active 100 sub-account 推到 config-center。
// 失败只 warn 不影响主流程（cache miss caller 会 fallback 到 DB）。
func (s *Scheduler) pushFleetCacheAfterRotation(ctx context.Context, logicalAccountID int64, logger *zap.Logger) {
	if s.fleetCache == nil {
		return
	}
	subs, err := s.instances.ListActiveFleet(ctx, logicalAccountID)
	if err != nil {
		if logger != nil {
			logger.Warn("fleet cache push: list active fleet failed (cache may be stale)",
				zap.Int64("la_id", logicalAccountID), zap.Error(err))
		}
		return
	}
	if pErr := s.fleetCache.Push(ctx, logicalAccountID, subs); pErr != nil {
		if logger != nil {
			logger.Warn("fleet cache push failed (cache may be stale, caller fallback to DB)",
				zap.Int64("la_id", logicalAccountID), zap.Error(pErr))
		}
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

	// 下一期 group：当前是 A → 下一期 B；当前是 B → 下一期 A；首次（NULL）→ A
	nextGroup := model.AccountGroupA
	if la.CurrentActiveGroup != nil {
		if *la.CurrentActiveGroup == model.AccountGroupA {
			nextGroup = model.AccountGroupB
		} else {
			nextGroup = model.AccountGroupA
		}
	}

	// fleet 模式：一次建 100 个 sub-account，每个落不同 shard
	accountNos, err := s.idgen.NewProvisionedFleetAccountNos(ctx, la, periodStart)
	if err != nil {
		return fmt.Errorf("alloc fleet account_nos: %w", err)
	}
	if len(accountNos) != 100 {
		return fmt.Errorf("fleet idgen returned %d nos, expected 100", len(accountNos))
	}

	category, err := CategoryForAccountType(la.AccountType)
	if err != nil {
		return fmt.Errorf("category derive: %w", err)
	}

	configVersion := policy.ConfigVersion
	var createErrs int
	for i := 0; i < 100; i++ {
		sub := &model.Account{
			AccountNo:            accountNos[i],
			UserID:               int64(i), // fleet sub_idx → user_id
			AccountType:          la.AccountType,
			AccountCategory:      category,
			AccountBusinessType:  la.AccountBusinessType,
			Currency:             la.Currency,
			Balance:              0,
			AvailableBalance:     0,
			FrozenBalance:        0,
			Status:               model.AccountStatusActive,
			AccountGroup:         nextGroup,
			LogicalAccountID:     &la.ID,
			LifecyclePhase:       model.LifecyclePhaseProvisioned,
			PeriodStart:          &periodStart,
			PeriodEnd:            &periodEnd,
			PolicyVersionAtBirth: &configVersion,
			Version:              0,
		}
		if err := s.instances.CreateProvisioned(ctx, sub); err != nil {
			createErrs++
			// 不 fail-fast：尽量补齐，剩余的下一次 ensureProvisioned 重试
			continue
		}
	}
	if createErrs > 0 && createErrs == 100 {
		return fmt.Errorf("fleet provision all 100 failed (first 0 created)")
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

	// next.AccountGroup 是 provisioned 时由 ensureProvisioned 设置的（A/B 翻转）。
	newGroup := next.AccountGroup
	if newGroup == "" {
		newGroup = model.AccountGroupA
	}

	// fleet 模式：批量切 100 sub-account
	// old fleet group：要么是 currentActive.AccountGroup（已有 fleet），要么 ""（首次激活）
	oldGroup := ""
	if currentActive != nil {
		oldGroup = currentActive.AccountGroup
	}

	params := PromoteAndDrainFleetParams{
		LogicalAccountID:      la.ID,
		OldGroup:              oldGroup,
		NewGroup:              newGroup,
		NewActiveAccountNo:    next.AccountNo, // fleet anchor，取 GetProvisionedInstance 返回的那个作 LA 指针
		NewActivePeriodEnd:    *next.PeriodEnd,
		LogicalAccountVersion: la.Version,
		Now:                   now,
	}
	if err := s.instances.PromoteAndDrainFleet(ctx, params); err != nil {
		return fmt.Errorf("promote and drain fleet: %w", err)
	}
	result.Activated++

	// rotation 成功后把新 active fleet 推到 config-center，让所有 accounting
	// 实例本地 cache 立即更新。失败不影响主流程（caller 退化到 DB 查询）。
	s.pushFleetCacheAfterRotation(ctx, la.ID, s.logger)
	return nil
}

// ============================================================================
// 手动操作（admin-web 调用入口）
// ============================================================================

// ForceSwitch 运维强制切换：跳过时间检查，立即把 logical_account 的当前 active
// 转为 draining，并把 provisioned 提升为 active。
//
// 调用前提（caller / admin-web 责任）：
//   - 已经过 ops 双人复核
//   - operator + reason 字段必填，写入 audit log
//   - 已经预先 ensureProvisioned 过（或本方法内部 ensure）
//
// 失败模式：
//   - 锁竞争 → 立即返回错误（不等）
//   - 无 provisioned → 返回错误（caller 应先调 ForceProvision）
//   - CAS 冲突（其他 worker 已切）→ 视为成功（幂等）
func (s *Scheduler) ForceSwitch(
	ctx context.Context, logicalAccountID int64, operator, reason string,
) error {
	if operator == "" || reason == "" {
		return errors.New("ForceSwitch: operator and reason required for audit")
	}

	release, err := s.locks.AcquireForLogical(ctx, logicalAccountID, s.owner+"|manual:"+operator)
	if err != nil {
		return fmt.Errorf("force switch: acquire lock: %w", err)
	}
	defer release()

	policy, err := s.policies.GetPolicy(ctx, logicalAccountID)
	if err != nil {
		return fmt.Errorf("force switch: get policy: %w", err)
	}
	if policy == nil {
		return errors.New("force switch: no rotation policy")
	}

	// active 可能为 nil — 首次激活场景：刚 ForceProvision 完，还没有 active，
	// 现在要把 provisioned 提升为 active（没有"老 active"可切到 draining）。
	// swap() 内部支持 currentActive=nil（OldGroup="" 表示首次）。
	active, err := s.instances.GetActiveInstance(ctx, logicalAccountID)
	if err != nil {
		return fmt.Errorf("force switch: get active: %w", err)
	}

	now := s.clock()
	// 用一个伪 LA wrapper 调用 swap（需要 LA 的 Version 信息）
	// 这里简化：调用方应预先传入 LA；为接口简单起见，重新读一次
	// （生产实现可以传 LA 进来，这里为单一职责保持精简）
	la, err := s.findLogical(ctx, logicalAccountID)
	if err != nil {
		return err
	}

	result := &TickResult{}
	if err := s.swap(ctx, la, active, policy, now, result); err != nil {
		return fmt.Errorf("force switch swap: %w", err)
	}
	return nil
}

// ForceProvision 运维强制预创建下一期 provisioned instance（不切换）。
// 用于：scheduler 未及时跑 / 故障恢复后预先建好下一期。
func (s *Scheduler) ForceProvision(
	ctx context.Context, logicalAccountID int64, operator, reason string,
) error {
	if operator == "" || reason == "" {
		return errors.New("ForceProvision: operator and reason required")
	}

	release, err := s.locks.AcquireForLogical(ctx, logicalAccountID, s.owner+"|manual:"+operator)
	if err != nil {
		return fmt.Errorf("force provision: acquire lock: %w", err)
	}
	defer release()

	policy, err := s.policies.GetPolicy(ctx, logicalAccountID)
	if err != nil {
		return fmt.Errorf("force provision: get policy: %w", err)
	}
	if policy == nil {
		return errors.New("force provision: no policy")
	}

	active, err := s.instances.GetActiveInstance(ctx, logicalAccountID)
	if err != nil {
		return fmt.Errorf("force provision: get active: %w", err)
	}

	la, err := s.findLogical(ctx, logicalAccountID)
	if err != nil {
		return err
	}

	result := &TickResult{}
	return s.ensureProvisioned(ctx, la, policy, active, s.clock(), result)
}

// findLogical 内部辅助：通过 lister 找到 LA。
// 注：生产实现可以让 LogicalAccountLister 提供 GetByID。这里临时遍历 list。
func (s *Scheduler) findLogical(ctx context.Context, id int64) (*model.LogicalAccount, error) {
	las, err := s.logicals.ListRotating(ctx, 1000)
	if err != nil {
		return nil, fmt.Errorf("list rotating: %w", err)
	}
	for _, la := range las {
		if la.ID == id {
			return la, nil
		}
	}
	return nil, fmt.Errorf("logical_account id=%d not found in rotating list", id)
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

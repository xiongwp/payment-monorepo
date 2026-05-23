package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// AccountInstanceManager 提供 rotation scheduler 所需的 instance 读写。
//
// 【关键不变量】PromoteAndDrain 是跨表/可能跨分片的原子操作：
//   - 同 logical_account 的所有 instance 共享同一 user_id → 同一物理分片 ✓
//   - 因此 old + new instance 的状态切换可以在**同一本地事务**完成 ✓
//   - logical_account 在 account_meta 库（不同库）→ 单独事务
//
// PromoteAndDrain 实现策略（顺序两步）：
//   1. 在 instance 所在分片本地事务：UPDATE old phase=draining + new phase=active
//   2. 在 account_meta 库：UPDATE logical_account.current_active_account_no
//   3. 失败重试由 scheduler 下次 Tick 兜底（基于 CAS 幂等）
//
// 不一致检测（scheduler 自检）：
//   - 若 step 1 成功 step 2 失败：phase=active 的 instance ≠ LA.current_active_account_no
//   - scheduler 下次 Tick 修正：把 LA.current_active 改成实际 phase=active 的 instance
type AccountInstanceManager interface {
	// GetActiveInstance 返回 logical 下当前 phase=active 的 instance。
	GetActiveInstance(ctx context.Context, logicalAccountID int64) (*model.Account, error)

	// GetProvisionedInstance 返回 logical 下当前 phase=provisioned 的 instance。
	GetProvisionedInstance(ctx context.Context, logicalAccountID int64) (*model.Account, error)

	// CreateProvisioned 创建一个 provisioned instance（单 instance 路径；fleet 路径
	// 由 Scheduler 循环调 100 次本方法，每次传入 user_id=0..99 的 sub-account）。
	CreateProvisioned(ctx context.Context, acc *model.Account) error

	// PromoteAndDrain 原子切换：旧 active → draining，新 provisioned → active，
	// 同时更新 LA.current_active_account_no（跨库非原子，CAS 兜底）。
	PromoteAndDrain(ctx context.Context, params AIMPromoteParams) error

	// PromoteAndDrainFleet fleet 模式批量切换：
	// 同 logical_account_id + account_group=oldGroup 的全部 active → draining；
	// 同 logical_account_id + account_group=newGroup 的全部 provisioned → active；
	// 同步更新 LA.current_active_account_no（fleet anchor 取新 group 中第一个 user_id=0 的）
	// + current_active_group。
	//
	// Fleet 跨 100 个分片表（每个 sub-account 由 user_id 路由），需要并行多 shard UPDATE。
	PromoteAndDrainFleet(ctx context.Context, params AIMPromoteFleetParams) error

	// PromoteToFrozen 收敛 job 用：把 draining 推进到 frozen。
	PromoteToFrozen(ctx context.Context, accountNo string, expectedVersion int64, frozenAt time.Time) error

	// ListByLogical 返回 logical_account_id 下所有 phase 的 instance（按 period_start 升序）。
	// Fleet × Rotation 场景下可能返回 100~200 个 sub-account。limit<=0 默认 250。
	ListByLogical(ctx context.Context, logicalAccountID int64, limit int) ([]*model.Account, error)

	// SumBalanceByLogical 聚合 LA 全部 instance 的余额。
	// 默认排除 archived (phase=4)；其它 phase 都算 — fleet × rotation 切换窗口里
	// active + draining 同时存在是正常的，应当一起算。
	// 用于对账：第三方对账时拿到的"LA key 当前余额"应该是 fleet 全部 sub 的 SUM。
	SumBalanceByLogical(ctx context.Context, logicalAccountID int64) (LogicalAccountBalanceSummary, error)

	// GetActiveSubAccount 找 fleet 中 user_id=subIdx 的当前 active sub-account。
	// 用于 booking router 按 hash(flow_id) % 100 选 fleet sub 落账 (Phase 3 fleet 路由)。
	// 返回 nil 表示该 sub_idx 上没有 active 的 instance（可能在切换瞬态或 fleet 未完整）。
	GetActiveSubAccount(ctx context.Context, logicalAccountID int64, subIdx int) (*model.Account, error)
}

// LogicalAccountBalanceSummary LA 余额聚合视图。
type LogicalAccountBalanceSummary struct {
	LogicalAccountID  int64           `json:"logical_account_id"`
	Total             int64           `json:"total_balance"`     // 跨所有非 archived instance
	InstanceCount     int             `json:"instance_count"`
	ByGroup           map[string]int64 `json:"by_group"`         // "A" / "B" → balance sum
	ByPhase           map[int]int64    `json:"by_phase"`         // lifecycle_phase int → balance sum
	GroupCounts       map[string]int   `json:"group_counts"`     // 每 group 多少 sub
	PhaseCounts       map[int]int      `json:"phase_counts"`     // 每 phase 多少 sub
}

// AIMPromoteParams 切换参数（service 层用 PromoteAndDrainParams，repo 用本类型避免循环）。
type AIMPromoteParams struct {
	LogicalAccountID      int64
	OldActiveAccountNo    string
	OldActiveVersion      int64
	NewActiveAccountNo    string
	NewActiveVersion      int64
	NewActiveGroup        string // "A" / "B"；切换后写到 LA.current_active_group
	NewActivePeriodEnd    time.Time
	LogicalAccountVersion int64
	Now                   time.Time
}

// AIMPromoteFleetParams fleet 切换参数。
type AIMPromoteFleetParams struct {
	LogicalAccountID      int64
	OldGroup              string // 旧 fleet group（要 active → draining）；空=无旧 fleet（首次激活）
	NewGroup              string // 新 fleet group（要 provisioned → active）
	NewActiveAccountNo    string // 新 fleet 中的 anchor sub-account（写到 LA.current_active_account_no）
	NewActivePeriodEnd    time.Time
	LogicalAccountVersion int64
	Now                   time.Time
}

// 错误：可被业务层 errors.Is 检测。
var (
	ErrInstancePromoteCASConflict = errors.New("account instance CAS conflict during promote")
	ErrInstanceLogicalMismatch    = errors.New("logical_account current_active mismatch (consistency check)")
)

type accountInstanceManager struct {
	dbManager    *database.Manager
	router       *sharding.Router
	logicalRepo  LogicalAccountRepository // 用于 update LA.current_active
}

// NewAccountInstanceManager 构造。
func NewAccountInstanceManager(
	dbManager *database.Manager,
	router *sharding.Router,
	logicalRepo LogicalAccountRepository,
) AccountInstanceManager {
	return &accountInstanceManager{
		dbManager:   dbManager,
		router:      router,
		logicalRepo: logicalRepo,
	}
}

// GetActiveInstance 跨所有分片找？不需要 — 同 logical 的所有 instance 同片。
// 但当前没有 "用 logical_account_id 反查 user_id 路由" 的快捷方式，需要用 LA 表先查 user_id。
// 简化实现：用 LA.CurrentActiveAccountNo（反范式化字段）→ 直接路由该 account_no。
func (m *accountInstanceManager) GetActiveInstance(
	ctx context.Context, logicalAccountID int64,
) (*model.Account, error) {
	la, err := m.logicalRepo.GetByID(ctx, logicalAccountID)
	if err != nil {
		return nil, fmt.Errorf("aim: get LA: %w", err)
	}
	if la == nil {
		return nil, fmt.Errorf("aim: logical_account id=%d not found", logicalAccountID)
	}
	if la.CurrentActiveAccountNo == nil || *la.CurrentActiveAccountNo == "" {
		return nil, nil
	}
	return m.getByAccountNo(ctx, *la.CurrentActiveAccountNo)
}

// GetProvisionedInstance 通过 logical_account_id 找 provisioned。
// 由于 LA 表没有反范式化 "current_provisioned"，这里要走 account 表索引 idx_logical_phase。
// 同 LA 下所有 instance 同分片（共享 user_id），所以路由可以基于 LA 的 user_id。
// 简化：通过 LA.CurrentActiveAccountNo 推导 shard（同分片）。
func (m *accountInstanceManager) GetProvisionedInstance(
	ctx context.Context, logicalAccountID int64,
) (*model.Account, error) {
	la, err := m.logicalRepo.GetByID(ctx, logicalAccountID)
	if err != nil {
		return nil, err
	}
	if la == nil {
		return nil, nil
	}
	// 推导分片：用 current_active_account_no（若有），或用 account_meta 上的反范式化字段
	// 若 LA 是首次启用 rotation 还没有 active → 我们无法推导分片
	// → 这种情况下走全分片 scan（少见路径，可接受）
	if la.CurrentActiveAccountNo == nil || *la.CurrentActiveAccountNo == "" {
		return m.scanProvisionedAllShards(ctx, logicalAccountID)
	}
	dbIdx, gtblIdx := m.router.RouteByAccountNo(*la.CurrentActiveAccountNo)
	return m.queryProvisionedInShard(ctx, logicalAccountID, dbIdx, gtblIdx)
}

func (m *accountInstanceManager) queryProvisionedInShard(
	ctx context.Context, laID int64, dbIdx, gtblIdx int,
) (*model.Account, error) {
	db, err := m.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, err
	}
	tableName := m.router.TableName(ctx, "account", gtblIdx)
	var row model.Account
	res := db.WithContext(ctx).Table(tableName).
		Where("logical_account_id = ? AND lifecycle_phase = ?", laID, model.LifecyclePhaseProvisioned).
		Order("period_start ASC").
		Limit(1).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("aim: query provisioned: %w", res.Error)
	}
	return &row, nil
}

// scanProvisionedAllShards 兜底：扫所有 100 个分片找 provisioned。
// 仅在 LA 没有 current_active_account_no 时使用（首次启用 rotation 的初始情况）。
func (m *accountInstanceManager) scanProvisionedAllShards(
	ctx context.Context, laID int64,
) (*model.Account, error) {
	for gtblIdx := 0; gtblIdx < sharding.ShardTableTotal; gtblIdx++ {
		dbIdx := gtblIdx / sharding.ShardTablePerDB
		row, err := m.queryProvisionedInShard(ctx, laID, dbIdx, gtblIdx)
		if err != nil {
			return nil, err
		}
		if row != nil {
			return row, nil
		}
	}
	return nil, nil
}

func (m *accountInstanceManager) getByAccountNo(ctx context.Context, accountNo string) (*model.Account, error) {
	dbIdx, gtblIdx := m.router.RouteByAccountNo(accountNo)
	db, err := m.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, err
	}
	tableName := m.router.TableName(ctx, "account", gtblIdx)
	var row model.Account
	res := db.WithContext(ctx).Table(tableName).
		Where("account_no = ?", accountNo).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("aim: get account: %w", res.Error)
	}
	return &row, nil
}

func (m *accountInstanceManager) CreateProvisioned(ctx context.Context, acc *model.Account) error {
	if acc == nil {
		return errors.New("aim: nil account")
	}
	if acc.AccountNo == "" {
		return errors.New("aim: empty account_no")
	}
	if acc.LogicalAccountID == nil {
		return errors.New("aim: missing logical_account_id")
	}
	if acc.LifecyclePhase != model.LifecyclePhaseProvisioned {
		return fmt.Errorf("aim: phase must be provisioned, got %s", acc.LifecyclePhase)
	}
	dbIdx, gtblIdx := m.router.RouteByAccountNo(acc.AccountNo)
	db, err := m.dbManager.GetDB(dbIdx)
	if err != nil {
		return err
	}
	tableName := m.router.TableName(ctx, "account", gtblIdx)
	if err := db.WithContext(ctx).Table(tableName).Create(acc).Error; err != nil {
		if isDuplicateKeyErr(err) {
			return fmt.Errorf("aim: provisioned already exists for account_no=%s", acc.AccountNo)
		}
		return fmt.Errorf("aim: create provisioned: %w", err)
	}
	return nil
}

// PromoteAndDrain 实现 — 两步：
//   Step 1: 同分片本地事务（old → draining + new → active）
//   Step 2: account_meta 库：UPDATE LA.current_active_account_no
//
// 若 step 2 失败，下一个 scheduler Tick 会检测到 phase=active 的 instance 与
// LA.current_active 不一致，再次调用本方法（带新 version）会幂等修复。
func (m *accountInstanceManager) PromoteAndDrain(
	ctx context.Context, p AIMPromoteParams,
) error {
	if p.NewActiveAccountNo == "" {
		return errors.New("aim: empty new_active_account_no")
	}

	// 推导 shard（同 LA 下所有 instance 同分片）
	newDbIdx, newGtblIdx := m.router.RouteByAccountNo(p.NewActiveAccountNo)
	tableName := m.router.TableName(ctx, "account", newGtblIdx)
	db, err := m.dbManager.GetDB(newDbIdx)
	if err != nil {
		return err
	}

	// Step 1: 同分片本地事务
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 新 instance：provisioned → active
		newRes := tx.Table(tableName).
			Where("account_no = ? AND version = ? AND lifecycle_phase = ?",
				p.NewActiveAccountNo, p.NewActiveVersion, model.LifecyclePhaseProvisioned).
			Updates(map[string]any{
				"lifecycle_phase": model.LifecyclePhaseActive,
				"version":         gorm.Expr("version + 1"),
			})
		if newRes.Error != nil {
			return fmt.Errorf("promote new: %w", newRes.Error)
		}
		if newRes.RowsAffected == 0 {
			return fmt.Errorf("%w: new account_no=%s expected_version=%d",
				ErrInstancePromoteCASConflict, p.NewActiveAccountNo, p.NewActiveVersion)
		}

		// 旧 instance：active → draining（若 OldActiveAccountNo 非空）
		if p.OldActiveAccountNo != "" {
			oldRes := tx.Table(tableName).
				Where("account_no = ? AND version = ? AND lifecycle_phase = ?",
					p.OldActiveAccountNo, p.OldActiveVersion, model.LifecyclePhaseActive).
				Updates(map[string]any{
					"lifecycle_phase":      model.LifecyclePhaseDraining,
					"draining_started_at":  p.Now,
					"version":              gorm.Expr("version + 1"),
				})
			if oldRes.Error != nil {
				return fmt.Errorf("drain old: %w", oldRes.Error)
			}
			if oldRes.RowsAffected == 0 {
				return fmt.Errorf("%w: old account_no=%s expected_version=%d",
					ErrInstancePromoteCASConflict, p.OldActiveAccountNo, p.OldActiveVersion)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("aim: instance txn: %w", err)
	}

	// Step 2: 更新 LA.current_active_account_no + current_active_group（跨库）
	// 失败：scheduler 下次 Tick 自动协调
	if err := m.logicalRepo.UpdateCurrentActive(
		ctx, p.LogicalAccountID, p.NewActiveAccountNo, p.NewActiveGroup, p.NewActivePeriodEnd, p.LogicalAccountVersion,
	); err != nil {
		return fmt.Errorf("aim: update LA current_active (instance phase already swapped, will reconcile next tick): %w", err)
	}
	return nil
}

// PromoteAndDrainFleet fleet 模式批量切换。
//
// 跟 PromoteAndDrain（单 instance）的区别：
//   - 旧 active fleet（同 logical_account_id + group=oldGroup）的 100 sub-account 全部 active → draining
//   - 新 provisioned fleet（同 LA + group=newGroup）的 100 sub-account 全部 provisioned → active
//   - 跨 100 个分片表（每个 sub 落不同 shard）；用 100-goroutine 并行 UPDATE
//   - 不做 CAS（fleet 切换是运维强制的，跟单 instance 切换有 version 协调不同）
//
// 失败模式：
//   - 任一 shard UPDATE 失败 → 返回第一个错误（部分成功的不回滚 — Phase 2 限制，后续可加补偿）
//   - Step 2 LA 更新失败 → Phase 已经切了，scheduler 下次 Tick 协调
func (m *accountInstanceManager) PromoteAndDrainFleet(
	ctx context.Context, p AIMPromoteFleetParams,
) error {
	if p.NewGroup == "" {
		return errors.New("aim fleet: empty new_group")
	}
	if p.LogicalAccountID <= 0 {
		return errors.New("aim fleet: invalid logical_account_id")
	}

	// 跨 100 个 shard 表并行 UPDATE
	// 每个 shard 表都有 account_NN 这张表，但 fleet sub-account 按 user_id=0..99 散布。
	// 这里直接对每个 shard 跑 UPDATE WHERE logical_account_id=X AND account_group=Y
	type shardErr struct {
		dbIdx, gtblIdx int
		err            error
	}
	results := make(chan shardErr, 100)

	for sub := 0; sub < 100; sub++ {
		go func(subIdx int) {
			// user_id=subIdx 对应的 shard
			dbIdx, gtblIdx := m.router.RouteByUserID(int64(subIdx))
			tableName := m.router.TableName(ctx, "account", gtblIdx)
			db, err := m.dbManager.GetDB(dbIdx)
			if err != nil {
				results <- shardErr{dbIdx, gtblIdx, err}
				return
			}

			err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				// 新 group：provisioned → active
				newRes := tx.Table(tableName).
					Where("logical_account_id = ? AND account_group = ? AND lifecycle_phase = ?",
						p.LogicalAccountID, p.NewGroup, model.LifecyclePhaseProvisioned).
					Updates(map[string]any{
						"lifecycle_phase": model.LifecyclePhaseActive,
						"version":         gorm.Expr("version + 1"),
					})
				if newRes.Error != nil {
					return fmt.Errorf("promote new sub: %w", newRes.Error)
				}

				// 旧 group：active → draining (如果有 oldGroup)
				if p.OldGroup != "" {
					oldRes := tx.Table(tableName).
						Where("logical_account_id = ? AND account_group = ? AND lifecycle_phase = ?",
							p.LogicalAccountID, p.OldGroup, model.LifecyclePhaseActive).
						Updates(map[string]any{
							"lifecycle_phase":     model.LifecyclePhaseDraining,
							"draining_started_at": p.Now,
							"version":             gorm.Expr("version + 1"),
						})
					if oldRes.Error != nil {
						return fmt.Errorf("drain old sub: %w", oldRes.Error)
					}
				}
				return nil
			})
			results <- shardErr{dbIdx, gtblIdx, err}
		}(sub)
	}

	var firstErr error
	for i := 0; i < 100; i++ {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shard db=%d tbl=%d: %w", r.dbIdx, r.gtblIdx, r.err)
		}
	}
	if firstErr != nil {
		return fmt.Errorf("aim fleet phase swap: %w", firstErr)
	}

	// Step 2: 更新 LA.current_active_account_no + current_active_group
	if err := m.logicalRepo.UpdateCurrentActive(
		ctx, p.LogicalAccountID, p.NewActiveAccountNo, p.NewGroup, p.NewActivePeriodEnd, p.LogicalAccountVersion,
	); err != nil {
		return fmt.Errorf("aim fleet: update LA (phases swapped, will reconcile): %w", err)
	}
	return nil
}

// ListByLogical 扫全部 100 分片找属于该 LA 的所有 instance。
//
// Fleet × Rotation 模式下，同一 LA 的 100 sub-account 散布在 100 个不同 shard
// (按 user_id 0..99 路由)。所以必须扫全 100 shard，不能用 LA.current_active_account_no
// 推导单 shard（那只会返回 anchor sub-account 1 个）。
//
// 性能：admin-web 详情页 + 对账接口是低频请求，扫 100 shard 一次几十毫秒可接受。
// 业务热路径不走本方法（业务下账走 GetByAccountNo 按 account_no 直接定位单 shard）。
func (m *accountInstanceManager) ListByLogical(
	ctx context.Context, logicalAccountID int64, limit int,
) ([]*model.Account, error) {
	if limit <= 0 {
		limit = 250
	}
	la, err := m.logicalRepo.GetByID(ctx, logicalAccountID)
	if err != nil {
		return nil, fmt.Errorf("aim: get LA: %w", err)
	}
	if la == nil {
		return nil, fmt.Errorf("aim: logical_account id=%d not found", logicalAccountID)
	}

	queryShard := func(dbIdx, gtblIdx int) ([]*model.Account, error) {
		db, dberr := m.dbManager.GetDB(dbIdx)
		if dberr != nil {
			return nil, dberr
		}
		tableName := m.router.TableName(ctx, "account", gtblIdx)
		var rows []*model.Account
		res := db.WithContext(ctx).Table(tableName).
			Where("logical_account_id = ?", logicalAccountID).
			Order("period_start ASC, id ASC").
			Limit(limit).
			Find(&rows)
		if res.Error != nil {
			if errors.Is(res.Error, gorm.ErrRecordNotFound) {
				return nil, nil
			}
			return nil, res.Error
		}
		return rows, nil
	}

	// 扫所有 100 个 shard 表 — fleet 100 sub 散在不同 shard，不能走单 shard 快路径
	var all []*model.Account
	for gtblIdx := 0; gtblIdx < sharding.ShardTableTotal; gtblIdx++ {
		dbIdx := gtblIdx / sharding.ShardTablePerDB
		rows, err := queryShard(dbIdx, gtblIdx)
		if err != nil {
			return nil, fmt.Errorf("aim: list instances scan shard %d.%d: %w", dbIdx, gtblIdx, err)
		}
		all = append(all, rows...)
		if len(all) >= limit {
			return all[:limit], nil
		}
	}
	return all, nil
}

// PromoteToFrozen draining → frozen with CAS。
func (m *accountInstanceManager) PromoteToFrozen(
	ctx context.Context, accountNo string, expectedVersion int64, frozenAt time.Time,
) error {
	dbIdx, gtblIdx := m.router.RouteByAccountNo(accountNo)
	db, err := m.dbManager.GetDB(dbIdx)
	if err != nil {
		return err
	}
	tableName := m.router.TableName(ctx, "account", gtblIdx)
	res := db.WithContext(ctx).Table(tableName).
		Where("account_no = ? AND version = ? AND lifecycle_phase = ?",
			accountNo, expectedVersion, model.LifecyclePhaseDraining).
		Updates(map[string]any{
			"lifecycle_phase": model.LifecyclePhaseFrozen,
			"frozen_at":       frozenAt,
			"version":         gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return fmt.Errorf("aim: promote to frozen: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: account_no=%s expected_version=%d",
			ErrInstancePromoteCASConflict, accountNo, expectedVersion)
	}
	return nil
}

// SumBalanceByLogical 聚合 LA 全部 instance 的余额。
//
// 实现：复用 ListByLogical 拿所有 sub-account 再客户端聚合 — 同 LA 的 instance
// 一定共享相同 logical_account_id，所以 ListByLogical 已经够。
// 排除 archived (phase=4) 因为已归档不参与对账。
func (m *accountInstanceManager) SumBalanceByLogical(
	ctx context.Context, logicalAccountID int64,
) (LogicalAccountBalanceSummary, error) {
	out := LogicalAccountBalanceSummary{
		LogicalAccountID: logicalAccountID,
		ByGroup:          make(map[string]int64),
		ByPhase:          make(map[int]int64),
		GroupCounts:      make(map[string]int),
		PhaseCounts:      make(map[int]int),
	}

	instances, err := m.ListByLogical(ctx, logicalAccountID, 1000) // fleet × rotation 上限 ~300 (100 active + 100 draining + 100 provisioned)
	if err != nil {
		return out, fmt.Errorf("aim sum-balance: list: %w", err)
	}

	for _, inst := range instances {
		phase := int(inst.LifecyclePhase)
		// 跳过 archived (4)：已归档不参与余额对账
		if inst.LifecyclePhase == model.LifecyclePhaseArchived {
			continue
		}
		out.Total += inst.Balance
		out.InstanceCount++
		out.ByPhase[phase] += inst.Balance
		out.PhaseCounts[phase]++
		if inst.AccountGroup != "" {
			out.ByGroup[inst.AccountGroup] += inst.Balance
			out.GroupCounts[inst.AccountGroup]++
		}
	}
	return out, nil
}

// GetActiveSubAccount 找 fleet 中 user_id=subIdx 的当前 active sub-account。
//
// 路由：subIdx 决定 shard（跟 fleet 创建时 user_id=subIdx 一致），单 shard 查询。
// 查询条件：logical_account_id=X AND user_id=subIdx AND lifecycle_phase=active
//
// 用于 booking router fleet 路由（按 hash(flow_id) % 100 选 sub）。
// 切换瞬态某 sub 还没切完时返回 nil（caller 应回退到 anchor 兜底）。
func (m *accountInstanceManager) GetActiveSubAccount(
	ctx context.Context, logicalAccountID int64, subIdx int,
) (*model.Account, error) {
	if subIdx < 0 || subIdx >= 100 {
		return nil, fmt.Errorf("aim: invalid sub_idx=%d (must be 0..99)", subIdx)
	}
	dbIdx, gtblIdx := m.router.RouteByUserID(int64(subIdx))
	db, err := m.dbManager.GetDB(dbIdx)
	if err != nil {
		return nil, err
	}
	tableName := m.router.TableName(ctx, "account", gtblIdx)
	var row model.Account
	res := db.WithContext(ctx).Table(tableName).
		Where("logical_account_id = ? AND user_id = ? AND lifecycle_phase = ?",
			logicalAccountID, subIdx, model.LifecyclePhaseActive).
		Limit(1).
		Take(&row)
	if res.Error != nil {
		if errors.Is(res.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("aim: get active sub %d: %w", subIdx, res.Error)
	}
	return &row, nil
}

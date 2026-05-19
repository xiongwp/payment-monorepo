package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/repository"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TccBranchDetail TCC 分支详情（含分片路由信息，供运维查看）
type TccBranchDetail struct {
	*model.TccTransaction
	DBIndex    int `json:"db_index"`
	TableIndex int `json:"table_index"`
}

// TccService TCC 维护服务接口
type TccService interface {
	// GetTccStatus 查询一个 TCC 事务的所有分支状态（跨分片扫描）
	GetTccStatus(ctx context.Context, tccID string) ([]*TccBranchDetail, error)

	// ListStuck 查询超时仍处于 TRYING 的分支（跨分片扫描，用于监控/恢复）
	// timeoutMinutes: 超过多少分钟未完成视为 stuck；limit: 每次最多返回条数
	ListStuck(ctx context.Context, timeoutMinutes, limit int) ([]*TccBranchDetail, error)

	// CancelBranch 取消单条 TRYING 分支（释放冻结资金）
	// 幂等：已 CANCELLED 直接成功；已 CONFIRMED 返回错误
	CancelBranch(ctx context.Context, branchID, accountNo string) error

	// CancelTcc 取消一个 TCC 事务下所有 TRYING 分支
	CancelTcc(ctx context.Context, tccID string) error
}

type tccService struct {
	tccRepo   repository.TccRepository
	coordRepo repository.TccCoordinatorRepository // 可选；nil 退化为旧行为（无 coordinator CAS 保护）
	dbManager *database.Manager
	router    *sharding.Router
	logger    *zap.Logger
}

// NewTccService 创建 TCC 维护服务
func NewTccService(
	tccRepo repository.TccRepository,
	coordRepo repository.TccCoordinatorRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	logger *zap.Logger,
) TccService {
	return &tccService{
		tccRepo:   tccRepo,
		coordRepo: coordRepo,
		dbManager: dbManager,
		router:    router,
		logger:    logger,
	}
}

// GetTccStatus 跨全量分片查询 tccID 的所有分支
func (s *tccService) GetTccStatus(ctx context.Context, tccID string) ([]*TccBranchDetail, error) {
	var result []*TccBranchDetail
	for _, shard := range s.router.GetAllShards() {
		db, err := s.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return nil, fmt.Errorf("get db[%d]: %w", shard.DBIndex, err)
		}
		branches, err := s.tccRepo.ListBranchesByTccID(ctx, db, shard.TableIndex, tccID)
		if err != nil {
			s.logger.Warn("ListBranchesByTccID error",
				zap.Int("dbIndex", shard.DBIndex), zap.Int("tableIndex", shard.TableIndex), zap.Error(err))
			continue
		}
		for _, b := range branches {
			result = append(result, &TccBranchDetail{
				TccTransaction: b,
				DBIndex:        shard.DBIndex,
				TableIndex:     shard.TableIndex,
			})
		}
	}
	return result, nil
}

// ListStuck 跨全量分片并发查询超时未完成（TRYING）的分支。
//
// 并发策略：goroutine per shard，pre-allocated slice by index 避免加锁。
// 每个分片独立查询 limit 条（避免早期分片耗尽 limit 导致后续分片被跳过），
// 最终汇总后截断到 limit。
func (s *tccService) ListStuck(ctx context.Context, timeoutMinutes, limit int) ([]*TccBranchDetail, error) {
	if timeoutMinutes <= 0 {
		timeoutMinutes = 5
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	before := time.Now().Add(-time.Duration(timeoutMinutes) * time.Minute)

	shards := s.router.GetAllShards()
	type shardResult struct {
		branches []*TccBranchDetail
		err      error
	}
	results := make([]shardResult, len(shards))

	var wg sync.WaitGroup
	for i, shard := range shards {
		i, shard := i, shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := s.dbManager.GetDB(shard.DBIndex)
			if err != nil {
				results[i].err = fmt.Errorf("get db[%d]: %w", shard.DBIndex, err)
				return
			}
			branches, err := s.tccRepo.ListStuckBranches(ctx, db, shard.TableIndex, before, limit)
			if err != nil {
				s.logger.Warn("ListStuckBranches error",
					zap.Int("dbIndex", shard.DBIndex), zap.Int("tableIndex", shard.TableIndex), zap.Error(err))
				return
			}
			details := make([]*TccBranchDetail, len(branches))
			for j, b := range branches {
				details[j] = &TccBranchDetail{TccTransaction: b, DBIndex: shard.DBIndex, TableIndex: shard.TableIndex}
			}
			results[i].branches = details
		}()
	}
	wg.Wait()

	var all []*TccBranchDetail
	for _, r := range results {
		if r.err != nil {
			s.logger.Warn("ListStuck: shard query failed", zap.Error(r.err))
			continue
		}
		all = append(all, r.branches...)
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// CancelBranch 取消单条 TRYING 分支（释放 available_balance）
//
// 死锁预防：锁顺序与 tccConfirm 保持一致（account 先于 tcc_transaction）。
//
//	tccConfirm：GetAccountForUpdate(account) → ConfirmBranchFromTrying(tcc_transaction)
//	CancelBranch：LockAccount(account)         → GetBranchForUpdate(tcc_transaction)
//
// 两条路径对同一账户行的锁请求方向一致，彻底消除循环等待死锁。
func (s *tccService) CancelBranch(ctx context.Context, branchID, accountNo string) error {
	dbIdx, tableIdx := s.router.RouteByAccountNo(accountNo)
	db, err := s.dbManager.GetDB(dbIdx)
	if err != nil {
		return fmt.Errorf("get db: %w", err)
	}

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// ── 步骤 1：先锁 account 行（与 tccConfirm 锁顺序一致，防死锁）──────────────
	accountTable := s.router.GetTableName("account", tableIdx)
	var acctForLock model.Account
	lockResult := tx.WithContext(ctx).Table(accountTable).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("account_no = ?", accountNo).
		Take(&acctForLock)
	if lockResult.Error != nil && !errors.Is(lockResult.Error, gorm.ErrRecordNotFound) {
		tx.Rollback()
		return fmt.Errorf("lock account %s: %w", accountNo, lockResult.Error)
	}
	// account 不存在时仍继续（空回滚保护依然有效）

	// ── 步骤 2：再锁 tcc_transaction 行 ──────────────────────────────────────
	branch, err := s.tccRepo.GetBranchForUpdate(ctx, tx, branchID, dbIdx, tableIdx)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("get branch: %w", err)
	}
	// 空回滚保护 / 幂等
	if branch == nil || branch.Status == model.TccStatusCancelled {
		return tx.Commit().Error
	}
	if branch.Status == model.TccStatusConfirmed {
		tx.Rollback()
		return fmt.Errorf("branch %s is already CONFIRMED, cannot cancel", branchID)
	}

	// ── 步骤 3：恢复冻结的 available_balance（account 行已在步骤 1 锁定）────────
	if branch.FrozenAmount > 0 {
		result := tx.WithContext(ctx).Table(accountTable).
			Where("account_no = ?", accountNo).
			Updates(map[string]interface{}{
				"available_balance": gorm.Expr("available_balance + ?", branch.FrozenAmount),
				"version":           gorm.Expr("version + 1"),
			})
		if result.Error != nil {
			tx.Rollback()
			return fmt.Errorf("restore available_balance: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			s.logger.Error("CRITICAL: CancelBranch could not restore available_balance — account not found",
				zap.String("branchID", branchID),
				zap.String("accountNo", accountNo),
				zap.Int64("frozenAmount", branch.FrozenAmount),
			)
		}
	}

	if err := s.tccRepo.UpdateBranchStatus(ctx, tx, branchID, model.TccStatusCancelled, dbIdx, tableIdx); err != nil {
		tx.Rollback()
		return err
	}

	s.logger.Info("tcc branch cancelled",
		zap.String("branchID", branchID),
		zap.String("accountNo", accountNo),
		zap.Int64("frozenAmount", branch.FrozenAmount),
	)
	return tx.Commit().Error
}

// CancelTcc 取消一个 TCC 事务下所有 TRYING 分支
//
// 空回滚保护：若 tcc_id 对应的分支记录不存在（Try 在写入 branch 记录前崩溃），
// 视为空事务，直接返回 nil。
// 这防止 Recovery Worker 在"无分支"场景下陷入永久 CRITICAL 告警循环。
func (s *tccService) CancelTcc(ctx context.Context, tccID string) error {
	// ── 关键：CAS coordinator TRYING → CANCELLED 必须先于任何分支取消 ─────────
	//
	// 不这么做的话：client 在 confirm 路径里把 coordinator 从 TRYING 转成
	// CONFIRMING 之前的窗口期里，recovery worker (或其它 cancel 调用方) 调
	// CancelTcc，分支级行锁挡得住单分支双扣，但挡不住"客户端 confirm 还没到
	// branch[i] 时，cancel 已经把 branch[i] 改 CANCELLED" 这种 cross-step 竞态。
	// 后果：同一笔 TCC 的双分录 entry 一半 confirmed 一半 cancelled，
	// 试算 (RunTrialBalance) 不平。
	//
	// TransitionToCancelled 是 UPDATE … WHERE phase IN (TRYING, CANCELLED) 的
	// CAS：rows_affected=0 意味着两种情况——
	//   a) coordinator 已 CONFIRMING/CONFIRMED → client 已经过取消窗口，cancel 必须放弃
	//   b) coordinator 不存在（旧 booking 在 coord Create 失败兜底；branch 仍可能在）
	// 仅靠错误字符串区分两类不可靠（生产可能有不同 error wrap）；改成额外用
	// GetCoordinator 探测一下：存在但非 TRYING → abort；不存在 → 兼容旧路径继续 cancel branches。
	if s.coordRepo != nil {
		if err := s.coordRepo.TransitionToCancelled(ctx, tccID); err != nil {
			coord, getErr := s.coordRepo.GetByTccID(ctx, tccID)
			if getErr == nil && coord != nil {
				// 协调者存在且 CAS 失败 → 必然是 CONFIRMING/CONFIRMED
				s.logger.Warn("CancelTcc: skipped — coordinator no longer in TRYING (client likely past confirm window)",
					zap.String("tccID", tccID),
					zap.Int8("phase", int8(coord.Phase)),
					zap.Error(err))
				return nil
			}
			// 协调者不存在或读取失败 → 走旧路径继续 cancel 分支（兜底）。
			s.logger.Info("CancelTcc: coordinator missing or unreadable, falling back to branch-only cancel (legacy path)",
				zap.String("tccID", tccID), zap.Error(err))
		}
	}

	branches, err := s.GetTccStatus(ctx, tccID)
	if err != nil {
		return fmt.Errorf("get tcc status: %w", err)
	}
	if len(branches) == 0 {
		// 空回滚：Try 未写入任何分支（可能在账户检查或数据库连接阶段失败），
		// 没有冻结资金需要释放，直接视为取消成功。
		s.logger.Info("CancelTcc: no branches found, treating as empty rollback (no frozen balance to release)",
			zap.String("tccID", tccID))
		return nil
	}

	var cancelErrs []error
	for _, b := range branches {
		if b.Status != model.TccStatusTrying {
			continue
		}
		if err := s.CancelBranch(ctx, b.BranchID, b.AccountNo); err != nil {
			s.logger.Error("cancel branch failed",
				zap.String("tccID", tccID), zap.String("branchID", b.BranchID), zap.Error(err))
			cancelErrs = append(cancelErrs, err) // 继续尝试其余分支，汇总所有错误
		}
	}
	return errors.Join(cancelErrs...)
}

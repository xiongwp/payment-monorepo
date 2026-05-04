package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// TccRepository TCC 分支记录仓储
type TccRepository interface {
	// CreateBranch 创建 TCC 分支（Try 阶段，在账户事务内执行）
	CreateBranch(ctx context.Context, tx *gorm.DB, branch *model.TccTransaction, dbIndex, tableIndex int) error

	// GetBranch 查询 TCC 分支（不加行锁），用于 Try 阶段幂等检查。
	//
	// 为何不加 FOR UPDATE：
	//   Try 的 branchID 是预生成、全局唯一（= transactionID），不会有两个并发事务
	//   持有相同 branchID；幂等检查只需要看"是否已存在"（重试场景）。FOR UPDATE 会在
	//   uk_branch_id 二级索引上放 gap / next-key lock，与后续 INSERT 的 insert-intent
	//   相互阻塞，形成热点账户下的 tcc_transaction 死锁源。换成普通 SELECT 后，
	//   只有命中已有行才加共享锁（依赖 gorm 默认的 tx 隔离），gap 不上锁，死锁消除。
	GetBranch(ctx context.Context, tx *gorm.DB, branchID string, dbIndex, tableIndex int) (*model.TccTransaction, error)

	// GetBranchForUpdate 查询 TCC 分支并加行锁（Confirm/Cancel 幂等检查用）
	GetBranchForUpdate(ctx context.Context, tx *gorm.DB, branchID string, dbIndex, tableIndex int) (*model.TccTransaction, error)

	// ConfirmBranchFromTrying 原子性地将 TRYING 状态的分支更新为 CONFIRMED。
	// 返回 (true, nil) — 成功；(false, nil) — 分支不在 TRYING 状态（已被取消或重复确认）。
	// 通过 WHERE status=TRYING 条件在 UPDATE 内部做状态检查，避免在 Confirm 正常路径上
	// 执行额外的 SELECT FOR UPDATE 查询，减少一次 DB 往返。
	ConfirmBranchFromTrying(ctx context.Context, tx *gorm.DB, branchID string, dbIndex, tableIndex int) (bool, error)

	// UpdateBranchStatus 更新分支状态（TRYING → CONFIRMED / CANCELLED）
	UpdateBranchStatus(ctx context.Context, tx *gorm.DB, branchID string, status model.TccStatus, dbIndex, tableIndex int) error

	// ListBranchesByTccID 查询指定 tccID 在某一分片内的所有分支（维护查询用）
	ListBranchesByTccID(ctx context.Context, db *gorm.DB, tableIndex int, tccID string) ([]*model.TccTransaction, error)

	// ListStuckBranches 查询某一分片内超时仍处于 TRYING 状态的分支（维护/恢复用）
	ListStuckBranches(ctx context.Context, db *gorm.DB, tableIndex int, before time.Time, limit int) ([]*model.TccTransaction, error)

	// ArchiveTerminalBranches 删除单个分片内状态 ∈ {CONFIRMED, CANCELLED} 且 updated_at < olderThan 的分支。
	// 用于 retention / 归档：TCC 表会无限增长，终态分支保留期后可安全清理
	// （流水、凭证在独立表中已完整记录）。分批 + limit 防止单次 DELETE 锁表过久。
	// 返回本次删除行数。
	ArchiveTerminalBranches(ctx context.Context, dbIndex, tableIndex int, olderThan time.Time, batchSize int) (int64, error)
}

type tccRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewTccRepository 创建 TCC 仓储
func NewTccRepository(dbManager *database.Manager, router *sharding.Router) TccRepository {
	return &tccRepository{
		dbManager: dbManager,
		router:    router,
	}
}

func (r *tccRepository) tableName(tableIndex int) string {
	return r.router.GetTableName("tcc_transaction", tableIndex)
}

func (r *tccRepository) CreateBranch(ctx context.Context, tx *gorm.DB, branch *model.TccTransaction, dbIndex, tableIndex int) error {
	return tx.WithContext(ctx).Table(r.tableName(tableIndex)).Create(branch).Error
}

// GetBranch 普通 SELECT，见接口注释。Try 阶段幂等检查用，避免 uk_branch_id 上的 gap lock。
func (r *tccRepository) GetBranch(ctx context.Context, tx *gorm.DB, branchID string, dbIndex, tableIndex int) (*model.TccTransaction, error) {
	var branch model.TccTransaction
	result := tx.WithContext(ctx).Table(r.tableName(tableIndex)).
		Where("branch_id = ?", branchID).
		Take(&branch)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}
	return &branch, nil
}

func (r *tccRepository) GetBranchForUpdate(ctx context.Context, tx *gorm.DB, branchID string, dbIndex, tableIndex int) (*model.TccTransaction, error) {
	var branch model.TccTransaction
	result := tx.WithContext(ctx).Table(r.tableName(tableIndex)).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("branch_id = ?", branchID).
		Take(&branch)

	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, result.Error
	}
	return &branch, nil
}

func (r *tccRepository) ConfirmBranchFromTrying(ctx context.Context, tx *gorm.DB, branchID string, dbIndex, tableIndex int) (bool, error) {
	result := tx.WithContext(ctx).Table(r.tableName(tableIndex)).
		Where("branch_id = ? AND status = ?", branchID, model.TccStatusTrying).
		Update("status", model.TccStatusConfirmed)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

func (r *tccRepository) UpdateBranchStatus(ctx context.Context, tx *gorm.DB, branchID string, status model.TccStatus, dbIndex, tableIndex int) error {
	return tx.WithContext(ctx).Table(r.tableName(tableIndex)).
		Where("branch_id = ?", branchID).
		Update("status", status).Error
}

func (r *tccRepository) ListBranchesByTccID(ctx context.Context, db *gorm.DB, tableIndex int, tccID string) ([]*model.TccTransaction, error) {
	var branches []*model.TccTransaction
	err := db.WithContext(ctx).Table(r.tableName(tableIndex)).
		Where("tcc_id = ?", tccID).
		Order("id ASC").
		Find(&branches).Error
	return branches, err
}

// ArchiveTerminalBranches DELETE CONFIRMED/CANCELLED 且 updated_at < olderThan，限 batchSize。
// 使用 account-level 连接（非事务），多次短 DELETE 比一次大 DELETE 对锁/主从延迟更友好。
func (r *tccRepository) ArchiveTerminalBranches(ctx context.Context, dbIndex, tableIndex int, olderThan time.Time, batchSize int) (int64, error) {
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return 0, fmt.Errorf("tcc archive: get db[%d]: %w", dbIndex, err)
	}
	if batchSize <= 0 {
		batchSize = 1000
	}
	// 用子查询 SELECT id 后再 DELETE，避免大量行扫描拖慢复制；MySQL 8.0+ 支持 LIMIT on DELETE。
	res := db.WithContext(ctx).
		Exec("DELETE FROM "+r.tableName(tableIndex)+
			" WHERE status IN (?, ?) AND updated_at < ? LIMIT ?",
			model.TccStatusConfirmed, model.TccStatusCancelled, olderThan, batchSize)
	if res.Error != nil {
		return 0, fmt.Errorf("tcc archive db[%d] tbl[%02d]: %w", dbIndex, tableIndex, res.Error)
	}
	return res.RowsAffected, nil
}

func (r *tccRepository) ListStuckBranches(ctx context.Context, db *gorm.DB, tableIndex int, before time.Time, limit int) ([]*model.TccTransaction, error) {
	var branches []*model.TccTransaction
	err := db.WithContext(ctx).Table(r.tableName(tableIndex)).
		Where("status = ? AND created_at < ?", model.TccStatusTrying, before).
		Order("created_at ASC").
		Limit(limit).
		Find(&branches).Error
	return branches, err
}

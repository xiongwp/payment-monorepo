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

// BalanceBufferRepository 账户余额缓冲聚合表仓储接口
//
// 适用账户：平台/中间账户，以及 buffer_account_config 中显式配置的账户。
// 写入时机：TCC Confirm 阶段，余额增量暂存至此表，由后台 flush worker 按 flush_scheduled_at 批量刷新到 account。
type BalanceBufferRepository interface {
	// Upsert adds delta to pending_delta for accountNo within an existing transaction.
	// flushScheduledAt is set only on INSERT (first transaction for this account);
	// subsequent upserts leave flush_scheduled_at unchanged so the scheduled task is not disturbed.
	Upsert(ctx context.Context, tx *gorm.DB, accountNo string, delta int64, tableIndex int, flushScheduledAt time.Time) error

	// FindDueForFlush returns up to `limit` rows where the scheduled flush time has arrived
	// (flush_scheduled_at <= now OR flush_scheduled_at IS NULL) or pending_count is high.
	// Results are ordered: high pending_count first, then oldest flush_scheduled_at first.
	FindDueForFlush(ctx context.Context, dbIndex, tableIndex int, limit int, now time.Time) ([]model.AccountBalanceBuffer, error)

	// LockRow SELECT FOR UPDATE; returns nil if row no longer exists (already flushed).
	LockRow(ctx context.Context, tx *gorm.DB, accountNo string, tableIndex int) (*model.AccountBalanceBuffer, error)

	// RescheduleRow resets pending counts to 0 and advances flush_scheduled_at by the given interval.
	// Used for buffer-configured accounts after a flush to maintain the recurring task pattern.
	RescheduleRow(ctx context.Context, tx *gorm.DB, accountNo string, tableIndex int, nextScheduledAt time.Time) error

	// DeleteRow removes the row after a successful flush (used for platform/transit accounts).
	DeleteRow(ctx context.Context, tx *gorm.DB, accountNo string, tableIndex int) error

	// GetPendingDelta returns the current pending_delta for accountNo; returns 0 if no row exists.
	GetPendingDelta(ctx context.Context, dbIndex, tableIndex int, accountNo string) (int64, error)
}

type balanceBufferRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewBalanceBufferRepository 创建余额缓冲仓储
func NewBalanceBufferRepository(dbManager *database.Manager, router *sharding.Router) BalanceBufferRepository {
	return &balanceBufferRepository{dbManager: dbManager, router: router}
}

// tableName 已弃用：旧版本不接 ctx，shadow 路径会落主表。新代码用 tableNameCtx。
// Deprecated: use tableNameCtx(ctx, tableIndex)
func (r *balanceBufferRepository) tableName(tableIndex int) string {
	return r.router.GetTableName("account_balance_buffer", tableIndex)
}

// tableNameCtx 按 ctx 解析主 / 影子表名（callback 不覆盖 raw Exec，必须显式带 ctx）。
func (r *balanceBufferRepository) tableNameCtx(ctx context.Context, tableIndex int) string {
	return r.router.TableName(ctx, "account_balance_buffer", tableIndex)
}

// Upsert 首次写入时同时设置 flush_scheduled_at（触发时间）；后续写入只累加 delta 和 count，不改变调度时间。
//
// **P2-2 raw SQL 安全性说明**：
// 这里用 raw `Exec` 而非 GORM `Clauses(clause.OnConflict{...})` 是有意的——
// MySQL `ON DUPLICATE KEY UPDATE` 的语义跟 GORM Clauses 自动生成的 SQL 在 increment
// 表达上有微妙差异（GORM 可能用 SELECT-then-UPDATE 而非原子 ON DUP KEY），高 QPS 下
// 我们必须保留单条原子 INSERT...ON DUP KEY 的语义。
//
// **shadow 安全**：tableName 已经由 r.tableNameCtx(ctx) 在调用前解出 _shadow 后缀，
// raw SQL 注入到指定 table 名上；GORM callback 框架不会路由本路径，但因为表名已经
// 是 ctx-aware 的，shadow 流量自然落到 _shadow 表。**未来添加新 raw Exec 路径必须**
// 同样调 tableNameCtx，否则 shadow 流量会污染主表。
func (r *balanceBufferRepository) Upsert(ctx context.Context, tx *gorm.DB, accountNo string, delta int64, tableIndex int, flushScheduledAt time.Time) error {
	tableName := r.tableNameCtx(ctx, tableIndex)
	return tx.WithContext(ctx).Exec(
		"INSERT INTO "+tableName+
			" (account_no, pending_delta, pending_count, flush_scheduled_at)"+
			" VALUES (?, ?, 1, ?)"+
			" ON DUPLICATE KEY UPDATE"+
			"   pending_delta = pending_delta + VALUES(pending_delta),"+
			"   pending_count = pending_count + 1",
		// flush_scheduled_at intentionally NOT updated on duplicate — preserves scheduled task time
		accountNo, delta, flushScheduledAt,
	).Error
}

// FindDueForFlush 查询已到期（flush_scheduled_at <= now 或 NULL）或高频（pending_count >= threshold）的条目。
// 返回最多 limit 条，优先返回高频条目，其次按 flush_scheduled_at 升序（最早到期优先）。
func (r *balanceBufferRepository) FindDueForFlush(ctx context.Context, dbIndex, tableIndex int, limit int, now time.Time) ([]model.AccountBalanceBuffer, error) {
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("balance_buffer FindDueForFlush: get db[%d]: %w", dbIndex, err)
	}
	var rows []model.AccountBalanceBuffer
	if err := db.WithContext(ctx).Table(r.tableNameCtx(ctx, tableIndex)).
		Where("flush_scheduled_at IS NULL OR flush_scheduled_at <= ? OR pending_count >= ?",
			now, model.BufferFlushThreshold).
		Order("pending_count DESC, flush_scheduled_at ASC").
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("balance_buffer FindDueForFlush: %w", err)
	}
	return rows, nil
}

// LockRow 在事务中锁定缓冲行，用于 flush worker 独占 drain。
//
// 使用 SKIP LOCKED（MySQL 8.0+）：若同一行已被另一个 flush worker / 实例持有 X 锁，
// 本次直接返回 nil（视作"已被别人刷了"）而不是阻塞等 innodb_lock_wait_timeout（50s 默认）。
// 配合 FindDueForFlush 的 ORDER BY，100 个分片上多实例并发刷新时，彼此不互相拖慢；
// 本轮错过的行会在下一个 flush tick 再次被捞起。
//
// 为什么不用普通 FOR UPDATE 的 NOWAIT:
//   - NOWAIT 冲突时立即报错（ER_LOCK_NOWAIT），需要手动处理 error code，冗长；
//   - SKIP LOCKED 更适合"工作队列"语义,直接返回空行,调用方已经有 locked == nil 的分支。
func (r *balanceBufferRepository) LockRow(ctx context.Context, tx *gorm.DB, accountNo string, tableIndex int) (*model.AccountBalanceBuffer, error) {
	var row model.AccountBalanceBuffer
	result := tx.WithContext(ctx).Table(r.tableNameCtx(ctx, tableIndex)).
		Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("account_no = ?", accountNo).
		Take(&row)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			// 两种情况都返回 nil，调用方幂等退出：
			//   1. 行真的不存在（别的 flush 已完成 DELETE）
			//   2. 行被其他 tx 锁住，SKIP LOCKED 跳过，MySQL 返回空结果集
			return nil, nil
		}
		return nil, fmt.Errorf("balance_buffer LockRow: %w", result.Error)
	}
	return &row, nil
}

// RescheduleRow 刷新完成后重置 pending 计数，并将 flush_scheduled_at 延后一个间隔。
// 保留行记录（不删除），形成循环调度的任务链。
func (r *balanceBufferRepository) RescheduleRow(ctx context.Context, tx *gorm.DB, accountNo string, tableIndex int, nextScheduledAt time.Time) error {
	return tx.WithContext(ctx).Table(r.tableNameCtx(ctx, tableIndex)).
		Where("account_no = ?", accountNo).
		Updates(map[string]interface{}{
			"pending_delta":     0,
			"pending_count":     0,
			"flush_scheduled_at": nextScheduledAt,
		}).Error
}

func (r *balanceBufferRepository) DeleteRow(ctx context.Context, tx *gorm.DB, accountNo string, tableIndex int) error {
	return tx.WithContext(ctx).Table(r.tableNameCtx(ctx, tableIndex)).
		Where("account_no = ?", accountNo).
		Delete(&model.AccountBalanceBuffer{}).Error
}

func (r *balanceBufferRepository) GetPendingDelta(ctx context.Context, dbIndex, tableIndex int, accountNo string) (int64, error) {
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return 0, fmt.Errorf("balance_buffer GetPendingDelta: get db[%d]: %w", dbIndex, err)
	}
	var row model.AccountBalanceBuffer
	result := db.WithContext(ctx).Table(r.tableNameCtx(ctx, tableIndex)).
		Where("account_no = ?", accountNo).
		Take(&row)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("balance_buffer GetPendingDelta: %w", result.Error)
	}
	return row.PendingDelta, nil
}

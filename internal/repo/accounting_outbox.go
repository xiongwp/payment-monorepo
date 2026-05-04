package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// AccountingOutboxRepository 记账 outbox 仓储。
//
// 按 payment_intent_id 路由：同一 PI 的 charge/refund/记账都落同一物理库，
// 方便联合查询。
type AccountingOutboxRepository interface {
	// Insert 入队一条 pending 行；若 request_id 已存在，返回原行 +
	// ErrAccountingOutboxDuplicate。
	Insert(ctx context.Context, row *domain.AccountingOutbox) (*domain.AccountingOutbox, error)
	// ListPending 扫描所有分片，取 pending 且 next_attempt_at<=now 的行。
	ListPending(ctx context.Context, now time.Time, limit int) ([]*domain.AccountingOutbox, error)
	// MarkSent 成功投递：status→sent + sent_at。
	MarkSent(ctx context.Context, row *domain.AccountingOutbox) error
	// PurgeSentBefore 删除 status=sent 且 sent_at < before 的行，每个分片最多 limit。
	// 返回跨所有分片的删除总数。dead-letter (status=failed) 不删，留给运维核查。
	PurgeSentBefore(ctx context.Context, before time.Time, limit int) (int64, error)
	// CountDeadLetters 统计 status=failed 的行数（跨分片）。供 metric 告警。
	CountDeadLetters(ctx context.Context) (int64, error)
	// MarkRetry 失败但可重试：attempts++ + next_attempt_at + last_error。
	MarkRetry(ctx context.Context, row *domain.AccountingOutbox, nextAt time.Time, errMsg string) error
	// MarkFailed 终态失败：status→failed + last_error。
	MarkFailed(ctx context.Context, row *domain.AccountingOutbox, errMsg string) error
}

type accountingOutboxRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewAccountingOutboxRepository 构造。
func NewAccountingOutboxRepository(mgr *Manager, r *sharding.Router) AccountingOutboxRepository {
	return &accountingOutboxRepo{mgr: mgr, router: r}
}

const accountingOutboxTable = "accounting_outbox"

func (r *accountingOutboxRepo) shardOf(piID string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.GetTableName(accountingOutboxTable, tblIdx), nil
}

func (r *accountingOutboxRepo) Insert(ctx context.Context, row *domain.AccountingOutbox) (*domain.AccountingOutbox, error) {
	if row.PaymentIntentID == "" || row.RequestID == "" {
		return nil, fmt.Errorf("%w: payment_intent_id and request_id required", domain.ErrValidation)
	}
	if row.Status == "" {
		row.Status = domain.AccountingOutboxPending
	}
	// 给 next_attempt_at 一个确定值（= now），避免 NULL。让 ListPending 的 WHERE
	// 条件从 "status=? AND (next_attempt_at IS NULL OR next_attempt_at<=?)" 简化为
	// "status=? AND next_attempt_at<=?"，直接吃 idx_status_next 复合索引，
	// 不再因 NULL 分支走全表扫。
	if row.NextAttemptAt == nil {
		now := time.Now()
		row.NextAttemptAt = &now
	}
	db, tbl, err := r.shardOf(row.PaymentIntentID)
	if err != nil {
		return nil, err
	}
	err = db.WithContext(ctx).Table(tbl).Create(row).Error
	if err == nil {
		return row, nil
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
		existing, getErr := r.getByRequestID(ctx, db, tbl, row.RequestID)
		if getErr != nil {
			return nil, fmt.Errorf("dedupe lookup after dup: %w (orig: %v)", getErr, err)
		}
		return existing, domain.ErrAccountingOutboxDuplicate
	}
	if strings.Contains(err.Error(), "Duplicate entry") {
		existing, getErr := r.getByRequestID(ctx, db, tbl, row.RequestID)
		if getErr != nil {
			return nil, fmt.Errorf("dedupe lookup after dup: %w (orig: %v)", getErr, err)
		}
		return existing, domain.ErrAccountingOutboxDuplicate
	}
	return nil, err
}

func (r *accountingOutboxRepo) getByRequestID(ctx context.Context, db *gorm.DB, tbl, reqID string) (*domain.AccountingOutbox, error) {
	var out domain.AccountingOutbox
	err := db.WithContext(ctx).Table(tbl).Where("request_id = ?", reqID).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("accounting outbox not found: %s", reqID)
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *accountingOutboxRepo) ListPending(ctx context.Context, now time.Time, limit int) ([]*domain.AccountingOutbox, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*domain.AccountingOutbox
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return nil, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.GetTableName(accountingOutboxTable, i*r.router.TablePerDB()+j)
			var rows []*domain.AccountingOutbox
			// 走 idx_status_next (status, next_attempt_at) 复合索引：
			//   - Insert 已保证 next_attempt_at 非 NULL → 主路径是等值+范围，吃满索引
			//   - IS NULL 兜底是兼容老数据（历史上曾存在 NULL 行），MySQL 会走
			//     index merge union，成本低且不影响新流量
			q := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxPending).
				Where("next_attempt_at <= ? OR next_attempt_at IS NULL", now).
				Order("next_attempt_at, created").
				Limit(limit)
			if err := q.Find(&rows).Error; err != nil {
				return nil, fmt.Errorf("list pending accounting outbox on %s: %w", tbl, err)
			}
			out = append(out, rows...)
			if len(out) >= limit {
				return out[:limit], nil
			}
		}
	}
	return out, nil
}

func (r *accountingOutboxRepo) MarkSent(ctx context.Context, row *domain.AccountingOutbox) error {
	db, tbl, err := r.shardOf(row.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"status":  domain.AccountingOutboxSent,
			"sent_at": gorm.Expr("CURRENT_TIMESTAMP(3)"),
		}).Error
}

func (r *accountingOutboxRepo) MarkRetry(ctx context.Context, row *domain.AccountingOutbox, nextAt time.Time, errMsg string) error {
	db, tbl, err := r.shardOf(row.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"attempts":        gorm.Expr("attempts + 1"),
			"next_attempt_at": nextAt,
			"last_error":      errMsg,
		}).Error
}

func (r *accountingOutboxRepo) MarkFailed(ctx context.Context, row *domain.AccountingOutbox, errMsg string) error {
	db, tbl, err := r.shardOf(row.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).
		Where("id = ?", row.ID).
		Updates(map[string]any{
			"status":     domain.AccountingOutboxFailed,
			"last_error": errMsg,
		}).Error
}

// PurgeSentBefore 按分片扫 accounting_outbox_*，删除 status=sent 且 sent_at < before 的行。
// 实现为每分片单独 DELETE ... LIMIT N，避免一锁锁一张大表。
func (r *accountingOutboxRepo) PurgeSentBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	var total int64
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return total, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.GetTableName(accountingOutboxTable, i*r.router.TablePerDB()+j)
			res := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxSent).
				Where("sent_at IS NOT NULL AND sent_at < ?", before).
				Limit(limit).
				Delete(&domain.AccountingOutbox{})
			if res.Error != nil {
				return total, fmt.Errorf("purge sent on %s: %w", tbl, res.Error)
			}
			total += res.RowsAffected
		}
	}
	return total, nil
}

// CountDeadLetters 统计所有分片里 status=failed 的行数（供 metric / 告警）。
func (r *accountingOutboxRepo) CountDeadLetters(ctx context.Context) (int64, error) {
	var total int64
	for i := 0; i < r.router.DBCount(); i++ {
		db, err := r.mgr.GetShard(i)
		if err != nil {
			return total, err
		}
		for j := 0; j < r.router.TablePerDB(); j++ {
			tbl := r.router.GetTableName(accountingOutboxTable, i*r.router.TablePerDB()+j)
			var cnt int64
			if err := db.WithContext(ctx).Table(tbl).
				Where("status = ?", domain.AccountingOutboxFailed).
				Count(&cnt).Error; err != nil {
				return total, fmt.Errorf("count failed on %s: %w", tbl, err)
			}
			total += cnt
		}
	}
	return total, nil
}

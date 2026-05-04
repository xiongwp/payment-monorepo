// Package repository — FreezeCompensateOutboxRepository 100 分片访问。
package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// FreezeCompensateOutboxRepository 跨 100 分片访问 freeze_compensate_outbox_NN
type FreezeCompensateOutboxRepository interface {
	// Insert 同 tx 写入 PENDING。tx 路由到 voucher_no 对应分片由调用方保证。
	Insert(ctx context.Context, tx *gorm.DB, row *model.FreezeCompensateOutbox) error

	// MarkDone CAS PENDING → DONE。0 rows = 已被 worker 抢；caller 需要感知。
	MarkDone(ctx context.Context, voucherNo string) (bool, error)

	// ClaimPending CAS PENDING → COMPENSATING（worker 抢锁）。0 rows = 已被别人抢或终态。
	ClaimPending(ctx context.Context, voucherNo string) (bool, error)

	// MarkCompensated COMPENSATING → DONE
	MarkCompensated(ctx context.Context, voucherNo string) error

	// MarkFailed COMPENSATING → FAILED
	MarkFailed(ctx context.Context, voucherNo string, errMsg string) error

	// IncrementRetry retry_count++ + reset status COMPENSATING → PENDING（让下轮重试）
	IncrementRetry(ctx context.Context, voucherNo string, errMsg string) error

	// ListPendingExpired 跨所有分片扫 status=PENDING 且 created_at < now-after 的行
	ListPendingExpired(ctx context.Context, after time.Duration, limit int) ([]*model.FreezeCompensateOutbox, error)
}

type freezeCompensateOutboxRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewFreezeCompensateOutboxRepository 构造
func NewFreezeCompensateOutboxRepository(dbManager *database.Manager, router *sharding.Router) FreezeCompensateOutboxRepository {
	return &freezeCompensateOutboxRepository{dbManager: dbManager, router: router}
}

// 路由：voucher_no 前 3 字符 = "DTT"，D=dbIdx, TT=tableIdx (0-99)
func (r *freezeCompensateOutboxRepository) routeByVoucher(voucherNo string) (int, int, error) {
	if len(voucherNo) < 3 {
		return 0, 0, fmt.Errorf("voucher_no too short: %s", voucherNo)
	}
	dbIdx, err := strconv.Atoi(voucherNo[:1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse dbIdx: %w", err)
	}
	tableIdx, err := strconv.Atoi(voucherNo[1:3])
	if err != nil {
		return 0, 0, fmt.Errorf("parse tableIdx: %w", err)
	}
	return dbIdx, tableIdx, nil
}

func (r *freezeCompensateOutboxRepository) tableName(tableIdx int) string {
	return r.router.GetTableName("freeze_compensate_outbox", tableIdx)
}

func (r *freezeCompensateOutboxRepository) Insert(ctx context.Context, tx *gorm.DB, row *model.FreezeCompensateOutbox) error {
	_, tableIdx, err := r.routeByVoucher(row.VoucherNo)
	if err != nil {
		return err
	}
	return tx.WithContext(ctx).Table(r.tableName(tableIdx)).Create(row).Error
}

func (r *freezeCompensateOutboxRepository) casStatus(ctx context.Context, voucherNo string,
	from, to model.FreezeCompensateStatus, extra map[string]interface{}) (bool, error) {
	dbIdx, tableIdx, err := r.routeByVoucher(voucherNo)
	if err != nil {
		return false, err
	}
	db, err := r.dbManager.GetDB(dbIdx)
	if err != nil {
		return false, err
	}
	updates := map[string]interface{}{"status": to}
	for k, v := range extra {
		updates[k] = v
	}
	res := db.WithContext(ctx).Table(r.tableName(tableIdx)).
		Where("voucher_no = ? AND status = ?", voucherNo, from).
		Updates(updates)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *freezeCompensateOutboxRepository) MarkDone(ctx context.Context, voucherNo string) (bool, error) {
	return r.casStatus(ctx, voucherNo, model.FreezeCompensateStatusPending, model.FreezeCompensateStatusDone, nil)
}

func (r *freezeCompensateOutboxRepository) ClaimPending(ctx context.Context, voucherNo string) (bool, error) {
	return r.casStatus(ctx, voucherNo, model.FreezeCompensateStatusPending, model.FreezeCompensateStatusCompensating, nil)
}

func (r *freezeCompensateOutboxRepository) MarkCompensated(ctx context.Context, voucherNo string) error {
	ok, err := r.casStatus(ctx, voucherNo, model.FreezeCompensateStatusCompensating, model.FreezeCompensateStatusDone, nil)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("MarkCompensated: row not in COMPENSATING (state machine violation)")
	}
	return nil
}

func (r *freezeCompensateOutboxRepository) MarkFailed(ctx context.Context, voucherNo string, errMsg string) error {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	ok, err := r.casStatus(ctx, voucherNo, model.FreezeCompensateStatusCompensating, model.FreezeCompensateStatusFailed,
		map[string]interface{}{"error_msg": errMsg})
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("MarkFailed: row not in COMPENSATING")
	}
	return nil
}

func (r *freezeCompensateOutboxRepository) IncrementRetry(ctx context.Context, voucherNo string, errMsg string) error {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	dbIdx, tableIdx, err := r.routeByVoucher(voucherNo)
	if err != nil {
		return err
	}
	db, err := r.dbManager.GetDB(dbIdx)
	if err != nil {
		return err
	}
	res := db.WithContext(ctx).Table(r.tableName(tableIdx)).
		Where("voucher_no = ? AND status = ?", voucherNo, model.FreezeCompensateStatusCompensating).
		Updates(map[string]interface{}{
			"status":      model.FreezeCompensateStatusPending,
			"retry_count": gorm.Expr("retry_count + 1"),
			"error_msg":   errMsg,
		})
	return res.Error
}

func (r *freezeCompensateOutboxRepository) ListPendingExpired(ctx context.Context, after time.Duration, limit int) ([]*model.FreezeCompensateOutbox, error) {
	cutoff := time.Now().Add(-after)
	var out []*model.FreezeCompensateOutbox
	for _, shard := range r.router.GetAllShards() {
		db, err := r.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return out, err
		}
		var rows []*model.FreezeCompensateOutbox
		if err := db.WithContext(ctx).Table(r.tableName(shard.TableIndex)).
			Where("status = ? AND created_at < ?", model.FreezeCompensateStatusPending, cutoff).
			Limit(limit).
			Find(&rows).Error; err != nil {
			return out, err
		}
		out = append(out, rows...)
		if len(out) >= limit {
			return out[:limit], nil
		}
	}
	return out, nil
}

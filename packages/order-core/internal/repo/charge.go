package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// ChargeRepository Charge 仓储（按 payment_intent_id 路由，与 PI 同分片）
type ChargeRepository interface {
	Create(ctx context.Context, c *domain.Charge) error
	Get(ctx context.Context, piID, chargeID string) (*domain.Charge, error)
	// GetForUpdate 行级锁读取（SELECT ... FOR UPDATE），用于退款金额检查防并发。
	// 必须在已有 GORM transaction 上下文里调用。
	GetForUpdate(ctx context.Context, tx *gorm.DB, piID, chargeID string) (*domain.Charge, error)
	// GetByChargeID 通过 ch_{dbIdx:1d}{tblIdx:02d}... 前缀解析分片位，跨 PI 定位
	GetByChargeID(ctx context.Context, chargeID string) (*domain.Charge, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.Charge, error)
	UpdateFields(ctx context.Context, piID, chargeID string, fields map[string]any) (*domain.Charge, error)
	// CASUpdateStatus 原子状态机推进：仅当当前 status==from 时更新到 to + 落 fields。
	// 返回 (ch, true) 表示本次调用赢得 transition；(ch, false) 表示状态已被另一并发
	// caller 改过 → 上层不应再做副作用（accounting outbox enqueue / amount 更新等）。
	//
	// 资损修复：之前 MarkSucceeded / MarkFailed 用非 CAS UpdateFields 改 charge.status，
	// 同时设 amount_captured / failure_code / paid 等。两个并发 caller（webhook +
	// ReconcileWorker，或 webhook 重传 + ChargeExpireWorker）可能用不同金额 / 不同
	// 终态各写一次：amount 被覆盖、status 被错误反转（succeeded → failed）。
	// 配套同 piRepo.UpdateStatus 的双 CAS：charge 行用 from-status 兜底，PI 行
	// 用现有 UpdateStatus 兜底；任一 race 输的 caller 不再触发副作用。
	CASUpdateStatus(ctx context.Context, piID, chargeID string,
		from, to domain.ChargeStatus, fields map[string]any) (*domain.Charge, bool, error)
	// ListExpired 跨分片扫描：status=pending 且 expired_at<now
	ListExpired(ctx context.Context, now time.Time, limit int) ([]*domain.Charge, error)
	// ListPendingForReconcile 跨分片扫描：status=pending/processing 且 created < before
	// （用于对账：长时间未终态的 Charge 调渠道 Query 查真实状态）
	ListPendingForReconcile(ctx context.Context, before time.Time, limit int) ([]*domain.Charge, error)
}

type chargeRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewChargeRepository 构造
func NewChargeRepository(mgr *Manager, r *sharding.Router) ChargeRepository {
	return &chargeRepo{mgr: mgr, router: r}
}

func (r *chargeRepo) shardOf(ctx context.Context, piID string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.TableName(ctx, "charge", tblIdx), nil
}

func (r *chargeRepo) Create(ctx context.Context, c *domain.Charge) error {
	if c.PaymentIntentID == "" {
		return fmt.Errorf("%w: payment_intent_id required", domain.ErrValidation)
	}
	db, tbl, err := r.shardOf(ctx, c.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).Create(c).Error
}

func (r *chargeRepo) Get(ctx context.Context, piID, chargeID string) (*domain.Charge, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var c domain.Charge
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, chargeID).First(&c).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrChargeNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (r *chargeRepo) GetForUpdate(ctx context.Context, tx *gorm.DB, piID, chargeID string) (*domain.Charge, error) {
	_, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var c domain.Charge
	err = tx.WithContext(ctx).Table(tbl).
		Set("gorm:query_option", "FOR UPDATE").
		Where("payment_intent_id = ? AND id = ?", piID, chargeID).First(&c).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrChargeNotFound
		}
		return nil, err
	}
	return &c, nil
}

// GetByChargeID ch_{dbIdx:1d}{tblIdx:02d}... 前缀解析分片位，跨 PI 定位
func (r *chargeRepo) GetByChargeID(ctx context.Context, chargeID string) (*domain.Charge, error) {
	if chargeID == "" {
		return nil, fmt.Errorf("%w: charge_id required", domain.ErrValidation)
	}
	dbIdx, tblIdx := r.router.RouteByPrefixedID(chargeID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, err
	}
	tbl := r.router.TableName(ctx, "charge", tblIdx)
	var c domain.Charge
	err = db.WithContext(ctx).Table(tbl).Where("id = ?", chargeID).First(&c).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrChargeNotFound
		}
		return nil, err
	}
	return &c, nil
}

func (r *chargeRepo) ListByPI(ctx context.Context, piID string) ([]*domain.Charge, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.Charge
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ?", piID).
		Order("created ASC").Find(&out).Error
	return out, err
}

// ListExpired 跨分片扫描过期 pending Charge
func (r *chargeRepo) ListExpired(ctx context.Context, now time.Time, limit int) ([]*domain.Charge, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return fanOutShards(ctx, r.router.AllShards(), limit,
		func(ctx context.Context, dbIdx, tblIdx int) ([]*domain.Charge, error) {
			db, err := r.mgr.GetShard(dbIdx)
			if err != nil {
				return nil, err
			}
			tbl := r.router.TableName(ctx, "charge", tblIdx)
			var part []*domain.Charge
			err = db.WithContext(ctx).Table(tbl).
				Where("status = ? AND expired_at IS NOT NULL AND expired_at < ?", domain.ChargeStatusPending, now).
				Limit(limit).Find(&part).Error
			return part, err
		})
}

// ListPendingForReconcile 扫描长时间卡在 pending 的 Charge（对账候选）
func (r *chargeRepo) ListPendingForReconcile(ctx context.Context, before time.Time, limit int) ([]*domain.Charge, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return fanOutShards(ctx, r.router.AllShards(), limit,
		func(ctx context.Context, dbIdx, tblIdx int) ([]*domain.Charge, error) {
			db, err := r.mgr.GetShard(dbIdx)
			if err != nil {
				return nil, err
			}
			tbl := r.router.TableName(ctx, "charge", tblIdx)
			var part []*domain.Charge
			err = db.WithContext(ctx).Table(tbl).
				Where("status = ? AND created < ?", domain.ChargeStatusPending, before).
				Limit(limit).Find(&part).Error
			return part, err
		})
}

func (r *chargeRepo) UpdateFields(ctx context.Context, piID, chargeID string, fields map[string]any) (*domain.Charge, error) {
	if len(fields) == 0 {
		return r.Get(ctx, piID, chargeID)
	}
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	res := db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, chargeID).Updates(fields)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, domain.ErrChargeNotFound
	}
	return r.Get(ctx, piID, chargeID)
}

// CASUpdateStatus 见接口注释。fields 里若包含 "status" 会被本函数覆盖成 to。
func (r *chargeRepo) CASUpdateStatus(ctx context.Context, piID, chargeID string,
	from, to domain.ChargeStatus, fields map[string]any) (*domain.Charge, bool, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, false, err
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["status"] = string(to)
	// CAS：WHERE 含 status=from。RowsAffected=0 → 行不存在 OR 状态已被并发推进。
	// 上层处理一致：拿当前 charge 行返回，但 won=false 让它 skip 副作用。
	res := db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ? AND status = ?", piID, chargeID, string(from)).
		Updates(fields)
	if res.Error != nil {
		return nil, false, res.Error
	}
	ch, getErr := r.Get(ctx, piID, chargeID)
	if getErr != nil {
		return nil, false, getErr
	}
	return ch, res.RowsAffected == 1, nil
}


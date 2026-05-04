package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// PayActionRepository PayAction 仓储（按 payment_intent_id 路由，与 PI / Charge / Refund 同分片）
type PayActionRepository interface {
	Create(ctx context.Context, a *domain.PayAction) error
	Get(ctx context.Context, piID, actionID string) (*domain.PayAction, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.PayAction, error)
	PendingByPI(ctx context.Context, piID string) (*domain.PayAction, error)
	UpdateFields(ctx context.Context, piID, actionID string, fields map[string]any) (*domain.PayAction, error)
	IncrementAttempt(ctx context.Context, piID, actionID string) error
}

type payActionRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewPayActionRepository 构造
func NewPayActionRepository(mgr *Manager, r *sharding.Router) PayActionRepository {
	return &payActionRepo{mgr: mgr, router: r}
}

func (r *payActionRepo) shardOf(ctx context.Context, piID string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.TableName(ctx, "pay_action", tblIdx), nil
}

func (r *payActionRepo) Create(ctx context.Context, a *domain.PayAction) error {
	if a.PaymentIntentID == "" {
		return fmt.Errorf("%w: payment_intent_id required", domain.ErrValidation)
	}
	db, tbl, err := r.shardOf(ctx, a.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).Create(a).Error
}

func (r *payActionRepo) Get(ctx context.Context, piID, id string) (*domain.PayAction, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var a domain.PayAction
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, id).First(&a).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrPayActionNotFound
		}
		return nil, err
	}
	return &a, nil
}

func (r *payActionRepo) ListByPI(ctx context.Context, piID string) ([]*domain.PayAction, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.PayAction
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ?", piID).
		Order("created DESC").Find(&out).Error
	return out, err
}

// PendingByPI 返回该 PI 下仍处于 pending 的动作（最多一条）
func (r *payActionRepo) PendingByPI(ctx context.Context, piID string) (*domain.PayAction, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var a domain.PayAction
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND status = ?", piID, domain.PayActionStatusPending).
		Order("created DESC").First(&a).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrPayActionNotFound
		}
		return nil, err
	}
	return &a, nil
}

func (r *payActionRepo) UpdateFields(ctx context.Context, piID, id string, fields map[string]any) (*domain.PayAction, error) {
	if len(fields) == 0 {
		return r.Get(ctx, piID, id)
	}
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	res := db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, id).Updates(fields)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, domain.ErrPayActionNotFound
	}
	return r.Get(ctx, piID, id)
}

func (r *payActionRepo) IncrementAttempt(ctx context.Context, piID, id string) error {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, id).
		UpdateColumn("attempt_count", gorm.Expr("attempt_count + 1")).Error
}

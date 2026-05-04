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

// NotifyLogRepository 通知日志仓储（按 payment_intent_id 路由）
type NotifyLogRepository interface {
	Create(ctx context.Context, n *domain.NotifyLog) error
	Get(ctx context.Context, piID, id string) (*domain.NotifyLog, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.NotifyLog, error)
	UpdateFields(ctx context.Context, piID, id string, fields map[string]any) (*domain.NotifyLog, error)
	// ListDue 跨分片扫描：status ∈ (pending/retrying) 且 (next_retry_at IS NULL 或 < now)
	ListDue(ctx context.Context, now time.Time, limit int) ([]*domain.NotifyLog, error)
}

type notifyLogRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewNotifyLogRepository 构造
func NewNotifyLogRepository(mgr *Manager, r *sharding.Router) NotifyLogRepository {
	return &notifyLogRepo{mgr: mgr, router: r}
}

func (r *notifyLogRepo) shardOf(piID string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.GetTableName("notify_log", tblIdx), nil
}

func (r *notifyLogRepo) Create(ctx context.Context, n *domain.NotifyLog) error {
	if n.PaymentIntentID == "" {
		return fmt.Errorf("%w: payment_intent_id required", domain.ErrValidation)
	}
	db, tbl, err := r.shardOf(n.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).Create(n).Error
}

func (r *notifyLogRepo) Get(ctx context.Context, piID, id string) (*domain.NotifyLog, error) {
	db, tbl, err := r.shardOf(piID)
	if err != nil {
		return nil, err
	}
	var n domain.NotifyLog
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, id).First(&n).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrNotifyLogNotFound
		}
		return nil, err
	}
	return &n, nil
}

func (r *notifyLogRepo) ListByPI(ctx context.Context, piID string) ([]*domain.NotifyLog, error) {
	db, tbl, err := r.shardOf(piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.NotifyLog
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ?", piID).
		Order("created DESC").Find(&out).Error
	return out, err
}

func (r *notifyLogRepo) UpdateFields(ctx context.Context, piID, id string, fields map[string]any) (*domain.NotifyLog, error) {
	if len(fields) == 0 {
		return r.Get(ctx, piID, id)
	}
	db, tbl, err := r.shardOf(piID)
	if err != nil {
		return nil, err
	}
	res := db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, id).Updates(fields)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, domain.ErrNotifyLogNotFound
	}
	return r.Get(ctx, piID, id)
}

// ListDue 跨分片扫描待发送 / 待重试的记录
func (r *notifyLogRepo) ListDue(ctx context.Context, now time.Time, limit int) ([]*domain.NotifyLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	dueStates := []string{string(domain.NotifyLogStatusPending), string(domain.NotifyLogStatusRetrying)}
	var out []*domain.NotifyLog
	for _, shard := range r.router.AllShards() {
		dbIdx, tblIdx := shard[0], shard[1]
		db, err := r.mgr.GetShard(dbIdx)
		if err != nil {
			return nil, err
		}
		tbl := r.router.GetTableName("notify_log", tblIdx)
		var part []*domain.NotifyLog
		err = db.WithContext(ctx).Table(tbl).
			Where("status IN ? AND (next_retry_at IS NULL OR next_retry_at < ?)", dueStates, now).
			Limit(limit).Find(&part).Error
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
		if len(out) >= limit {
			out = out[:limit]
			break
		}
	}
	return out, nil
}

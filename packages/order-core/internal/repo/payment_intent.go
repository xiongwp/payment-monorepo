package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/sharding"
)

// PaymentIntentRepository PaymentIntent 仓储
type PaymentIntentRepository interface {
	Create(ctx context.Context, pi *domain.PaymentIntent) error
	Get(ctx context.Context, id string) (*domain.PaymentIntent, error)
	// GetByIdempotencyKey 按 (mch_id, idempotency_key) 查询首次创建结果；key 为空直接返回 not-found
	GetByIdempotencyKey(ctx context.Context, mchID, businessID, key string) (*domain.PaymentIntent, error)
	UpdateStatus(ctx context.Context, id string, from, to domain.PaymentIntentStatus, mutate func(*domain.PaymentIntent)) (*domain.PaymentIntent, error)
	UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.PaymentIntent, error)
	List(ctx context.Context, mchID string, page, size int) ([]*domain.PaymentIntent, int64, error)
	// ListExpired 扫描所有分片找出 expired_at < now 且仍处于非终态的 PI（cron 关单用）
	ListExpired(ctx context.Context, now time.Time, limit int) ([]*domain.PaymentIntent, error)
}

type piRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewPaymentIntentRepository 构造
func NewPaymentIntentRepository(mgr *Manager, r *sharding.Router) PaymentIntentRepository {
	return &piRepo{mgr: mgr, router: r}
}

func (r *piRepo) shardOf(id string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(id)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.GetTableName("payment_intent", tblIdx), nil
}

func (r *piRepo) Create(ctx context.Context, pi *domain.PaymentIntent) error {
	db, tbl, err := r.shardOf(pi.ID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).Create(pi).Error
}

func (r *piRepo) Get(ctx context.Context, id string) (*domain.PaymentIntent, error) {
	db, tbl, err := r.shardOf(id)
	if err != nil {
		return nil, err
	}
	var pi domain.PaymentIntent
	err = db.WithContext(ctx).Table(tbl).Where("id = ?", id).First(&pi).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrPaymentIntentNotFound
		}
		return nil, err
	}
	return &pi, nil
}

// GetByIdempotencyKey 幂等键查询。路由键与 Create 时保持一致（business_id 优先，fallback mch_id）。
func (r *piRepo) GetByIdempotencyKey(ctx context.Context, mchID, businessID, key string) (*domain.PaymentIntent, error) {
	if key == "" {
		return nil, domain.ErrPaymentIntentNotFound
	}
	routeKey := businessID
	if routeKey == "" {
		routeKey = mchID
	}
	dbIdx, tblIdx := r.router.RouteByString(routeKey)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, err
	}
	tbl := r.router.GetTableName("payment_intent", tblIdx)
	var pi domain.PaymentIntent
	err = db.WithContext(ctx).Table(tbl).
		Where("mch_id = ? AND idempotency_key = ?", mchID, key).
		First(&pi).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrPaymentIntentNotFound
		}
		return nil, err
	}
	return &pi, nil
}

func (r *piRepo) UpdateStatus(ctx context.Context, id string, from, to domain.PaymentIntentStatus, mutate func(*domain.PaymentIntent)) (*domain.PaymentIntent, error) {
	db, tbl, err := r.shardOf(id)
	if err != nil {
		return nil, err
	}
	var out domain.PaymentIntent
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 用 SELECT ... FOR UPDATE 锁住目标行：防两个并发 UpdateStatus 都通过
		// 状态校验、都创建 Charge / 都触发 accounting_outbox → 重复扣款 + 重复入账。
		// 之前用 First (无锁) + Save (id 唯一键 UPDATE) 是 race：两个 caller 都能
		// 读到 from 状态，都能 Save 成 to 状态，仅最后一次 Save 落地，但中间副作用
		// (createCharge / outbox enqueue) 都已经各自执行了一遍。
		var pi domain.PaymentIntent
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Table(tbl).Where("id = ?", id).First(&pi).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return domain.ErrPaymentIntentNotFound
			}
			return err
		}
		if from != "" && pi.Status != from {
			return fmt.Errorf("%w: expected=%s actual=%s", domain.ErrInvalidTransition, from, pi.Status)
		}
		if !domain.CanTransition(pi.Status, to) {
			return fmt.Errorf("%w: %s → %s", domain.ErrInvalidTransition, pi.Status, to)
		}
		pi.Status = to
		pi.Updated = time.Now().UTC()
		if mutate != nil {
			mutate(&pi)
		}
		// CAS-style UPDATE：WHERE 含 status=from 双保险（即便 FOR UPDATE 行锁
		// 失效，UPDATE 也能通过 rows_affected=0 检测出并发干扰）。
		// 用 Updates(map) 而非 Save(struct)，确保 gorm 生成的 SQL 含我们的
		// "id = ? AND status = ?" 整个 WHERE 子句（Save 在不同 gorm 版本对
		// Where 子句行为不一致）。
		// 列出 mutate 可能修改 + 状态机转换时一定要写的字段。其它字段保持不变。
		// 注意：必须用 Updates(map) 而不是 Save(struct)，因为 Save 在某些 gorm 版本
		// 下会忽略 Where 子句中除主键外的额外条件。
		updates := map[string]interface{}{
			"status":            pi.Status,
			"updated":           pi.Updated,
			"payment_method":    pi.PaymentMethod,
			"amount_received":   pi.AmountReceived,
			"amount_capturable": pi.AmountCapturable,
			"active_charge_ids": pi.ActiveChargeIDs,
			"active_refund_ids": pi.ActiveRefundIDs,
			"refund_phase":      pi.RefundPhase,
		}
		// 防御性 where 条件：from="" 时只 WHERE id（兼容历史调用方传空 from）
		whereCond := "id = ?"
		whereArgs := []interface{}{id}
		if from != "" {
			whereCond = "id = ? AND status = ?"
			whereArgs = []interface{}{id, from}
		}
		res := tx.Table(tbl).Where(whereCond, whereArgs...).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 && from != "" {
			return fmt.Errorf("%w: row vanished between SELECT and UPDATE (concurrent transition)", domain.ErrInvalidTransition)
		}
		out = pi
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *piRepo) UpdateFields(ctx context.Context, id string, fields map[string]any) (*domain.PaymentIntent, error) {
	if len(fields) == 0 {
		return r.Get(ctx, id)
	}
	db, tbl, err := r.shardOf(id)
	if err != nil {
		return nil, err
	}
	res := db.WithContext(ctx).Table(tbl).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, domain.ErrPaymentIntentNotFound
	}
	return r.Get(ctx, id)
}

func (r *piRepo) List(ctx context.Context, mchID string, page, size int) ([]*domain.PaymentIntent, int64, error) {
	if mchID == "" {
		return nil, 0, fmt.Errorf("%w: mch_id required", domain.ErrValidation)
	}
	dbIdx, tblIdx := r.router.RouteByString(mchID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, 0, err
	}
	tbl := r.router.GetTableName("payment_intent", tblIdx)
	if page <= 0 {
		page = 1
	}
	if size <= 0 || size > 500 {
		size = 20
	}
	var total int64
	if err := db.WithContext(ctx).Table(tbl).Where("mch_id = ?", mchID).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var list []*domain.PaymentIntent
	err = db.WithContext(ctx).Table(tbl).Where("mch_id = ?", mchID).
		Order("created DESC").Offset((page - 1) * size).Limit(size).Find(&list).Error
	return list, total, err
}

func (r *piRepo) ListExpired(ctx context.Context, now time.Time, limit int) ([]*domain.PaymentIntent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	openStates := []string{
		string(domain.PIStatusCreated),
		string(domain.PIStatusRequiresAction),
		string(domain.PIStatusProcessing),
	}
	return fanOutShards(ctx, r.router.AllShards(), limit,
		func(ctx context.Context, dbIdx, tblIdx int) ([]*domain.PaymentIntent, error) {
			db, err := r.mgr.GetShard(dbIdx)
			if err != nil {
				return nil, err
			}
			tbl := r.router.GetTableName("payment_intent", tblIdx)
			var part []*domain.PaymentIntent
			err = db.WithContext(ctx).Table(tbl).
				Where("expired_at IS NOT NULL AND expired_at < ? AND status IN ?", now, openStates).
				Limit(limit).Find(&part).Error
			return part, err
		})
}

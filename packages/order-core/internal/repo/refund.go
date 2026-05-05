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

// RefundRepository Refund 仓储（按 payment_intent_id 路由，与 PI / Charge 同分片）
type RefundRepository interface {
	Create(ctx context.Context, rf *domain.Refund) error
	Get(ctx context.Context, piID, refundID string) (*domain.Refund, error)
	// GetByIdempotencyKey 按 (pi_id, idempotency_key) 查；命中即返已有 refund，不命中返 ErrRefundNotFound。
	// 给 Create 路径做幂等：同 (pi, key) 重复请求直接返首次结果，不建第二条。
	GetByIdempotencyKey(ctx context.Context, piID, idempotencyKey string) (*domain.Refund, error)
	// GetByRefundID 通过 re_ 前缀直接路由
	GetByRefundID(ctx context.Context, refundID string) (*domain.Refund, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.Refund, error)
	ListByCharge(ctx context.Context, piID, chargeID string) ([]*domain.Refund, error)
	SumSucceededByCharge(ctx context.Context, piID, chargeID string) (int64, error)
	// SumActiveByCharge 累加 PENDING + SUCCEEDED；用于 Create 时的超额防护。
	SumActiveByCharge(ctx context.Context, piID, chargeID string) (int64, error)
	UpdateFields(ctx context.Context, piID, refundID string, fields map[string]any) (*domain.Refund, error)
	// CASUpdateStatus 原子状态机推进：仅当当前 status==from 时更新到 to，
	// 同时落 fields。返回 (rf, true) 表示本次调用赢得 transition；返回 (rf, false)
	// 表示状态已被另一个并发 caller 改过（or 行已是终态），上层不应再做副作用
	// （比如累加 amount_refunded、enqueue accounting outbox），避免双计 / 双写。
	//
	// 资损修复：之前 refundService.MarkSucceeded 用非 CAS 的 UpdateFields 改
	// status，紧接着 SQL `amount_refunded = amount_refunded + ?` 累加 charge
	// 余额；并发 webhook + RefundRetryWorker 各调一次 → 累加跑两次 → 余额翻倍
	// → 后续 Refund 被 SumActiveByCharge 拒掉 → 商户无法继续退款。
	CASUpdateStatus(ctx context.Context, piID, refundID string,
		from, to domain.RefundStatus, fields map[string]any) (*domain.Refund, bool, error)
	// ListRetryDue 扫描所有分片，找出需要重试的退款单（auto_compensate=1 或 status=failed
	// 且 next_retry_at < now）
	ListRetryDue(ctx context.Context, now time.Time, limit int) ([]*domain.Refund, error)
}

type refundRepo struct {
	mgr    *Manager
	router *sharding.Router
}

// NewRefundRepository 构造
func NewRefundRepository(mgr *Manager, r *sharding.Router) RefundRepository {
	return &refundRepo{mgr: mgr, router: r}
}

func (r *refundRepo) shardOf(ctx context.Context, piID string) (*gorm.DB, string, error) {
	dbIdx, tblIdx := r.router.RouteByPrefixedID(piID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, "", err
	}
	return db, r.router.TableName(ctx, "refund", tblIdx), nil
}

func (r *refundRepo) Create(ctx context.Context, rf *domain.Refund) error {
	if rf.PaymentIntentID == "" {
		return fmt.Errorf("%w: payment_intent_id required", domain.ErrValidation)
	}
	db, tbl, err := r.shardOf(ctx, rf.PaymentIntentID)
	if err != nil {
		return err
	}
	return db.WithContext(ctx).Table(tbl).Create(rf).Error
}

func (r *refundRepo) Get(ctx context.Context, piID, refundID string) (*domain.Refund, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var rf domain.Refund
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, refundID).First(&rf).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrRefundNotFound
		}
		return nil, err
	}
	return &rf, nil
}

func (r *refundRepo) GetByIdempotencyKey(ctx context.Context, piID, idempotencyKey string) (*domain.Refund, error) {
	if piID == "" || idempotencyKey == "" {
		return nil, domain.ErrRefundNotFound
	}
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var rf domain.Refund
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND idempotency_key = ?", piID, idempotencyKey).
		First(&rf).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrRefundNotFound
		}
		return nil, err
	}
	return &rf, nil
}

// GetByRefundID re_ 前缀路由
func (r *refundRepo) GetByRefundID(ctx context.Context, refundID string) (*domain.Refund, error) {
	if refundID == "" {
		return nil, fmt.Errorf("%w: refund_id required", domain.ErrValidation)
	}
	dbIdx, tblIdx := r.router.RouteByPrefixedID(refundID)
	db, err := r.mgr.GetShard(dbIdx)
	if err != nil {
		return nil, err
	}
	tbl := r.router.TableName(ctx, "refund", tblIdx)
	var rf domain.Refund
	err = db.WithContext(ctx).Table(tbl).Where("id = ?", refundID).First(&rf).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, domain.ErrRefundNotFound
		}
		return nil, err
	}
	return &rf, nil
}

// ListRetryDue 跨分片扫描：status=pending/failed + next_retry_at<=now
// 自动补偿（auto_compensate=1）即使 status=failed 也会继续重试
func (r *refundRepo) ListRetryDue(ctx context.Context, now time.Time, limit int) ([]*domain.Refund, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []*domain.Refund
	for _, sh := range r.router.AllShards() {
		db, err := r.mgr.GetShard(sh[0])
		if err != nil {
			return nil, err
		}
		tbl := r.router.TableName(ctx, "refund", sh[1])
		var part []*domain.Refund
		// 条件：
		//   (status=pending AND (next_retry_at IS NULL OR next_retry_at<=now))
		//   OR (status=failed AND auto_compensate=1 AND (next_retry_at IS NULL OR next_retry_at<=now))
		err = db.WithContext(ctx).Table(tbl).
			Where(`(status = ? AND (next_retry_at IS NULL OR next_retry_at <= ?))
			       OR (status = ? AND auto_compensate = 1 AND (next_retry_at IS NULL OR next_retry_at <= ?))`,
				domain.RefundStatusPending, now, domain.RefundStatusFailed, now).
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

func (r *refundRepo) ListByPI(ctx context.Context, piID string) ([]*domain.Refund, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.Refund
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ?", piID).
		Order("created DESC").Find(&out).Error
	return out, err
}

func (r *refundRepo) ListByCharge(ctx context.Context, piID, chargeID string) ([]*domain.Refund, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	var out []*domain.Refund
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND charge_id = ?", piID, chargeID).
		Order("created DESC").Find(&out).Error
	return out, err
}

func (r *refundRepo) SumSucceededByCharge(ctx context.Context, piID, chargeID string) (int64, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return 0, err
	}
	var total int64
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND charge_id = ? AND status = ?", piID, chargeID, domain.RefundStatusSucceeded).
		Select("COALESCE(SUM(amount), 0)").Scan(&total).Error
	return total, err
}

// SumActiveByCharge 累加 status IN (PENDING, SUCCEEDED) 的 refund 金额。
//
// 用于退款创建时的"超额退款"防护：必须把进行中的 PENDING refund 也算进去，
// 否则两个并发 Create 会都通过校验（各自看到 succeeded=0），都创建 PENDING，
// 都向 channel 发起，累计退款超过 amount_captured。
//
// 在调用方使用 FOR UPDATE 锁住 charge 行的 tx 内调用，与 INSERT refund 形成
// "原子检查-后插入"模式。
func (r *refundRepo) SumActiveByCharge(ctx context.Context, piID, chargeID string) (int64, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return 0, err
	}
	var total int64
	err = db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND charge_id = ? AND status IN ?",
			piID, chargeID, []domain.RefundStatus{domain.RefundStatusPending, domain.RefundStatusSucceeded}).
		Select("COALESCE(SUM(amount), 0)").Scan(&total).Error
	return total, err
}

func (r *refundRepo) UpdateFields(ctx context.Context, piID, refundID string, fields map[string]any) (*domain.Refund, error) {
	if len(fields) == 0 {
		return r.Get(ctx, piID, refundID)
	}
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, err
	}
	res := db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ?", piID, refundID).Updates(fields)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, domain.ErrRefundNotFound
	}
	return r.Get(ctx, piID, refundID)
}

// CASUpdateStatus 见接口注释。fields 里若包含 "status" 会被本函数覆盖成 to。
func (r *refundRepo) CASUpdateStatus(ctx context.Context, piID, refundID string,
	from, to domain.RefundStatus, fields map[string]any) (*domain.Refund, bool, error) {
	db, tbl, err := r.shardOf(ctx, piID)
	if err != nil {
		return nil, false, err
	}
	if fields == nil {
		fields = map[string]any{}
	}
	fields["status"] = string(to)
	// CAS：WHERE 含 status=from。RowsAffected=0 表示行不存在 OR 状态不匹配
	// （另一并发已经把它推进过了）。两种 case 上层处理一致：不再做副作用。
	res := db.WithContext(ctx).Table(tbl).
		Where("payment_intent_id = ? AND id = ? AND status = ?", piID, refundID, string(from)).
		Updates(fields)
	if res.Error != nil {
		return nil, false, res.Error
	}
	rf, getErr := r.Get(ctx, piID, refundID)
	if getErr != nil {
		// 行被并发 Cancel/Delete 等罕见路径删除——CAS 视作未赢。
		return nil, false, getErr
	}
	return rf, res.RowsAffected == 1, nil
}

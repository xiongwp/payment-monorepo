package repo

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/user-merchant-core/internal/domain"
)

const tblIdempotency = "idempotency_key"

func idempotencyTable(ctx context.Context) string { return shadow.TableName(ctx, tblIdempotency) }

// IdempotencyRepository 读写 idempotency_key 表。
type IdempotencyRepository interface {
	// Get：返回 (record, true, nil) 表示命中；(nil, false, nil) 表示 miss；
	// error 仅限 DB 故障。过期记录视为 miss（lazy eviction）。
	Get(ctx context.Context, key, method string) (*domain.IdempotencyRecord, bool, error)
	// TryInsert：第一次提交时写入。同 key 存在时返回 ErrIdempotencyExists，
	// 调用方应当立即 Get 一次拿现成结果（处理并发首发）。
	TryInsert(ctx context.Context, rec *domain.IdempotencyRecord) error
	// GC 删过期。cron 每小时跑一次即可，即使不跑也正常（Get 做 lazy check）。
	GC(ctx context.Context, before time.Time) (int64, error)
}

// ErrIdempotencyExists INSERT 时撞主键；调用方应转为 Get。
var ErrIdempotencyExists = errors.New("idempotency record exists")

type idempotencyRepo struct{ mgr *Manager }

// NewIdempotencyRepository 构造
func NewIdempotencyRepository(mgr *Manager) IdempotencyRepository {
	return &idempotencyRepo{mgr: mgr}
}

func (r *idempotencyRepo) db() *gorm.DB { return r.mgr.GetMeta() }

func (r *idempotencyRepo) Get(ctx context.Context, key, method string) (*domain.IdempotencyRecord, bool, error) {
	tbl := idempotencyTable(ctx)
	var rec domain.IdempotencyRecord
	err := r.db().WithContext(ctx).Table(tbl).
		Where("idempotency_key = ? AND method = ?", key, method).
		First(&rec).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// 过期视为 miss，同时异步清掉（不卡主路径）。
	// async goroutine 用 WithoutCancel 保留 shadow flag 但脱离 cancel 链。
	if !rec.Expires.IsZero() && time.Now().After(rec.Expires) {
		asyncCtx := shadow.WithoutCancel(ctx)
		go func() {
			_ = r.db().WithContext(asyncCtx).Table(idempotencyTable(asyncCtx)).
				Where("idempotency_key = ? AND method = ?", key, method).
				Delete(&domain.IdempotencyRecord{}).Error
		}()
		return nil, false, nil
	}
	return &rec, true, nil
}

func (r *idempotencyRepo) TryInsert(ctx context.Context, rec *domain.IdempotencyRecord) error {
	// ON CONFLICT DO NOTHING：撞 PK 不报错，靠 RowsAffected 判断是否首发。
	res := r.db().WithContext(ctx).Table(idempotencyTable(ctx)).
		Clauses(clause.OnConflict{DoNothing: true}).Create(rec)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrIdempotencyExists
	}
	return nil
}

func (r *idempotencyRepo) GC(ctx context.Context, before time.Time) (int64, error) {
	res := r.db().WithContext(ctx).Table(idempotencyTable(ctx)).
		Where("expires < ?", before).
		Delete(&domain.IdempotencyRecord{})
	return res.RowsAffected, res.Error
}

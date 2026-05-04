package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

const tblMerchantSecret = "merchant_channel_secret"

// MerchantSecretRepository 商户渠道凭据（密文）存储。
// 按 merchant_id 分片到 user_merchant_db_0..9 的 merchant_channel_secret_NN_(_shadow) 表。
type MerchantSecretRepository interface {
	Upsert(ctx context.Context, s *domain.MerchantChannelSecret) (*domain.MerchantChannelSecret, error)
	Get(ctx context.Context, merchantID, channel, fieldName string) (*domain.MerchantChannelSecret, error)
	ListByMerchantChannel(ctx context.Context, merchantID, channel string) ([]*domain.MerchantChannelSecret, error)
	ListByMerchant(ctx context.Context, merchantID string) ([]*domain.MerchantChannelSecret, error)
	Delete(ctx context.Context, merchantID, channel, fieldName string) error
}

type merchantSecretRepo struct {
	mgr    *Manager
	router *ShardRouter
}

// NewMerchantSecretRepository 构造。router 为 nil 时退化到单 meta DB（dev / 测试）。
func NewMerchantSecretRepository(mgr *Manager, router *ShardRouter) MerchantSecretRepository {
	return &merchantSecretRepo{mgr: mgr, router: router}
}

// shard 路由 helper：所有 CRUD 都按 merchant_id 路由。
func (r *merchantSecretRepo) shard(ctx context.Context, merchantID string) (*gorm.DB, string) {
	return shardForMerchant(ctx, r.mgr, r.router, tblMerchantSecret, merchantID)
}

func (r *merchantSecretRepo) Upsert(ctx context.Context, s *domain.MerchantChannelSecret) (*domain.MerchantChannelSecret, error) {
	if s.MerchantID == "" || s.Channel == "" || s.FieldName == "" || len(s.Ciphertext) == 0 {
		return nil, fmt.Errorf("%w: merchant_id/channel/field_name/ciphertext required", domain.ErrValidation)
	}
	db, tbl := r.shard(ctx, s.MerchantID)
	err := db.WithContext(ctx).Table(tbl).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "merchant_id"}, {Name: "channel"}, {Name: "field_name"}},
		DoUpdates: clause.Assignments(map[string]any{
			"ciphertext":  s.Ciphertext,
			"context":     s.Context,
			"masked_hint": s.MaskedHint,
			"version":     gorm.Expr("version + 1"),
			"created_by":  s.CreatedBy,
		}),
	}).Create(s).Error
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, s.MerchantID, s.Channel, s.FieldName)
}

func (r *merchantSecretRepo) Get(ctx context.Context, merchantID, channel, fieldName string) (*domain.MerchantChannelSecret, error) {
	db, tbl := r.shard(ctx, merchantID)
	var s domain.MerchantChannelSecret
	err := db.WithContext(ctx).Table(tbl).
		Where("merchant_id = ? AND channel = ? AND field_name = ?", merchantID, channel, fieldName).
		First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, domain.ErrMerchantSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *merchantSecretRepo) ListByMerchantChannel(ctx context.Context, merchantID, channel string) ([]*domain.MerchantChannelSecret, error) {
	db, tbl := r.shard(ctx, merchantID)
	var out []*domain.MerchantChannelSecret
	err := db.WithContext(ctx).Table(tbl).
		Where("merchant_id = ? AND channel = ?", merchantID, channel).
		Order("field_name ASC").Find(&out).Error
	return out, err
}

func (r *merchantSecretRepo) ListByMerchant(ctx context.Context, merchantID string) ([]*domain.MerchantChannelSecret, error) {
	db, tbl := r.shard(ctx, merchantID)
	var out []*domain.MerchantChannelSecret
	err := db.WithContext(ctx).Table(tbl).
		Where("merchant_id = ?", merchantID).
		Order("channel ASC, field_name ASC").Find(&out).Error
	return out, err
}

func (r *merchantSecretRepo) Delete(ctx context.Context, merchantID, channel, fieldName string) error {
	db, tbl := r.shard(ctx, merchantID)
	return db.WithContext(ctx).Table(tbl).
		Where("merchant_id = ? AND channel = ? AND field_name = ?", merchantID, channel, fieldName).
		Delete(&domain.MerchantChannelSecret{}).Error
}

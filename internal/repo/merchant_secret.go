package repo

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/xiongwp/user-merchant-core/internal/domain"
)

// MerchantSecretRepository 商户渠道凭据（密文）存储。non-sharded（meta DB）。
type MerchantSecretRepository interface {
	// Upsert 按 (merchant_id, channel, field_name) UPSERT；version 自增。
	Upsert(ctx context.Context, s *domain.MerchantChannelSecret) (*domain.MerchantChannelSecret, error)
	Get(ctx context.Context, merchantID, channel, fieldName string) (*domain.MerchantChannelSecret, error)
	// ListByMerchantChannel 列出某商户在某渠道的所有字段（原文不返）
	ListByMerchantChannel(ctx context.Context, merchantID, channel string) ([]*domain.MerchantChannelSecret, error)
	ListByMerchant(ctx context.Context, merchantID string) ([]*domain.MerchantChannelSecret, error)
	Delete(ctx context.Context, merchantID, channel, fieldName string) error
}

type merchantSecretRepo struct{ mgr *Manager }

// NewMerchantSecretRepository 构造
func NewMerchantSecretRepository(mgr *Manager) MerchantSecretRepository {
	return &merchantSecretRepo{mgr: mgr}
}

func (r *merchantSecretRepo) db() *gorm.DB   { return r.mgr.GetMeta() }
func (r *merchantSecretRepo) dbRO() *gorm.DB { return r.mgr.GetMetaRO() }

func (r *merchantSecretRepo) Upsert(ctx context.Context, s *domain.MerchantChannelSecret) (*domain.MerchantChannelSecret, error) {
	if s.MerchantID == "" || s.Channel == "" || s.FieldName == "" || len(s.Ciphertext) == 0 {
		return nil, fmt.Errorf("%w: merchant_id/channel/field_name/ciphertext required", domain.ErrValidation)
	}
	// Use ON DUPLICATE KEY UPDATE to bump version + refresh ciphertext.
	err := r.db().WithContext(ctx).Clauses(clause.OnConflict{
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
	var s domain.MerchantChannelSecret
	err := r.db().WithContext(ctx).
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
	var out []*domain.MerchantChannelSecret
	err := r.dbRO().WithContext(ctx).
		Where("merchant_id = ? AND channel = ?", merchantID, channel).
		Order("field_name ASC").Find(&out).Error
	return out, err
}

func (r *merchantSecretRepo) ListByMerchant(ctx context.Context, merchantID string) ([]*domain.MerchantChannelSecret, error) {
	var out []*domain.MerchantChannelSecret
	err := r.dbRO().WithContext(ctx).
		Where("merchant_id = ?", merchantID).
		Order("channel ASC, field_name ASC").Find(&out).Error
	return out, err
}

func (r *merchantSecretRepo) Delete(ctx context.Context, merchantID, channel, fieldName string) error {
	return r.db().WithContext(ctx).
		Where("merchant_id = ? AND channel = ? AND field_name = ?", merchantID, channel, fieldName).
		Delete(&domain.MerchantChannelSecret{}).Error
}

// Package repo — vault GORM repository.

package repo

import (
	"context"
	"errors"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/store"
)

type GormRepo struct {
	db  *gorm.DB
	log *zap.Logger
}

func NewGormRepo(db *gorm.DB, log *zap.Logger) *GormRepo {
	return &GormRepo{db: db, log: log}
}

func (r *GormRepo) Create(t domain.InternalToken) error {
	m := domain.FromInternalToken(t)
	return r.db.WithContext(context.Background()).Create(&m).Error
}

func (r *GormRepo) Get(token string) (domain.InternalToken, error) {
	var m domain.InternalTokenGormModel
	err := r.db.Where("token = ?", token).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.InternalToken{}, store.ErrNotFound
	}
	if err != nil {
		return domain.InternalToken{}, err
	}
	return m.ToDomain(), nil
}

func (r *GormRepo) FindByMerchantPANHash(merchantID, panHash string) (domain.InternalToken, error) {
	var m domain.InternalTokenGormModel
	err := r.db.Where("merchant_id = ? AND pan_hash = ?", merchantID, panHash).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.InternalToken{}, store.ErrNotFound
	}
	if err != nil {
		return domain.InternalToken{}, err
	}
	return m.ToDomain(), nil
}

func (r *GormRepo) FindByNetworkRef(provider domain.TokenProvider, ref string) (domain.InternalToken, error) {
	var m domain.InternalTokenGormModel
	err := r.db.Where("provider = ? AND token_ref_id = ?", string(provider), ref).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.InternalToken{}, store.ErrNotFound
	}
	if err != nil {
		return domain.InternalToken{}, err
	}
	return m.ToDomain(), nil
}

func (r *GormRepo) UpdateNetworkRef(token string, ref domain.NetworkRef) error {
	res := r.db.Model(&domain.InternalTokenGormModel{}).
		Where("token = ?", token).
		Updates(map[string]interface{}{
			"provider":       string(ref.Provider),
			"network_token":  ref.NetworkToken,
			"token_ref_id":   ref.TokenRefID,
			"token_expiry":   ref.TokenExpiry,
			"provisioned_at": ref.ProvisionedAt,
		})
	if res.RowsAffected == 0 {
		return store.ErrNotFound
	}
	return res.Error
}

func (r *GormRepo) UpdateStatus(token string, st domain.TokenStatus) error {
	res := r.db.Model(&domain.InternalTokenGormModel{}).
		Where("token = ?", token).
		Update("status", string(st))
	if res.RowsAffected == 0 {
		return store.ErrNotFound
	}
	return res.Error
}

func (r *GormRepo) GetEncryptedPAN(token string) (string, error) {
	var m domain.EncryptedPANGormModel
	err := r.db.Where("token = ?", token).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return m.Ciphertext, nil
}

func (r *GormRepo) StoreEncryptedPAN(token, ct string) error {
	return r.db.Save(&domain.EncryptedPANGormModel{Token: token, Ciphertext: ct}).Error
}

func (r *GormRepo) DeleteEncryptedPAN(token string) error {
	return r.db.Delete(&domain.EncryptedPANGormModel{}, "token = ?", token).Error
}

func (r *GormRepo) CountByMerchant(merchantID string) (int, error) {
	var n int64
	err := r.db.Model(&domain.InternalTokenGormModel{}).
		Where("merchant_id = ? AND status = ?", merchantID, "active").
		Count(&n).Error
	return int(n), err
}

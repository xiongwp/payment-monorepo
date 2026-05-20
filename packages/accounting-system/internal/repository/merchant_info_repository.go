package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// MerchantInfoRepository 商户信息仓储接口
type MerchantInfoRepository interface {
	// GetByMerchantID retrieves merchant info by merchantID. Returns nil if not found.
	GetByMerchantID(ctx context.Context, merchantID int64) (*model.MerchantInfo, error)
	// GetByExternalIDAndMid retrieves by (external_merchant_id, mid). Returns nil if not found.
	GetByExternalIDAndMid(ctx context.Context, externalMerchantID, mid string) (*model.MerchantInfo, error)
	// Create inserts a new merchant record.
	Create(ctx context.Context, info *model.MerchantInfo) error
	// Update updates all fields of an existing merchant record (by merchant_id).
	Update(ctx context.Context, info *model.MerchantInfo) error
}

type merchantInfoRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewMerchantInfoRepository 创建商户信息仓储
func NewMerchantInfoRepository(dbManager *database.Manager, router *sharding.Router) MerchantInfoRepository {
	return &merchantInfoRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// GetByMerchantID retrieves merchant info by merchantID. Returns nil if not found.
func (r *merchantInfoRepository) GetByMerchantID(ctx context.Context, merchantID int64) (*model.MerchantInfo, error) {
	dbIndex, tableIndex := r.router.RouteByID(merchantID)
	tableName := fmt.Sprintf("merchant_info_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("merchant_info GetByMerchantID: get db[%d]: %w", dbIndex, err)
	}

	var info model.MerchantInfo
	result := db.WithContext(ctx).Table(tableName).
		Where("merchant_id = ?", merchantID).
		First(&info)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("merchant_info GetByMerchantID: %w", result.Error)
	}

	return &info, nil
}

// GetByExternalIDAndMid retrieves by (external_merchant_id, mid).
// Scans all shards sequentially since external_id has no encoded routing info.
// Returns nil if not found.
func (r *merchantInfoRepository) GetByExternalIDAndMid(ctx context.Context, externalMerchantID, mid string) (*model.MerchantInfo, error) {
	for _, shard := range r.router.GetAllShards() {
		tableName := fmt.Sprintf("merchant_info_%02d", shard.TableIndex)

		db, err := r.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return nil, fmt.Errorf("merchant_info GetByExternalIDAndMid: get db[%d]: %w", shard.DBIndex, err)
		}

		var info model.MerchantInfo
		result := db.WithContext(ctx).Table(tableName).
			Where("external_merchant_id = ? AND mid = ?", externalMerchantID, mid).
			First(&info)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, fmt.Errorf("merchant_info GetByExternalIDAndMid: db[%d] table %s: %w",
				shard.DBIndex, tableName, result.Error)
		}
		return &info, nil
	}
	return nil, nil
}

// Create inserts a new merchant record.
func (r *merchantInfoRepository) Create(ctx context.Context, info *model.MerchantInfo) error {
	dbIndex, tableIndex := r.router.RouteByID(info.MerchantID)
	tableName := fmt.Sprintf("merchant_info_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("merchant_info Create: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).Create(info).Error
}

// Update updates all fields of an existing merchant record (by merchant_id).
func (r *merchantInfoRepository) Update(ctx context.Context, info *model.MerchantInfo) error {
	dbIndex, tableIndex := r.router.RouteByID(info.MerchantID)
	tableName := fmt.Sprintf("merchant_info_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("merchant_info Update: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Where("merchant_id = ?", info.MerchantID).
		Save(info).Error
}

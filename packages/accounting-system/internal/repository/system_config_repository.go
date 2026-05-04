package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SystemConfigRepository 系统通用 key-value 配置仓储（meta DB）。
type SystemConfigRepository interface {
	// ListAll 返回全表（启动时 + reload 时全量加载到内存 cache）。
	ListAll(ctx context.Context) ([]*model.SystemConfig, error)
	// GetByKey 单 key 查询。Not found 返回 nil, nil。
	GetByKey(ctx context.Context, key string) (*model.SystemConfig, error)
	// Upsert 插入或更新单条配置。description / value_type 可空（不覆盖已有的）。
	Upsert(ctx context.Context, cfg *model.SystemConfig) error
	// Delete 删除一条配置。返回是否真的删了行。
	Delete(ctx context.Context, key string) (bool, error)
}

type systemConfigRepository struct {
	dbManager *database.Manager
}

func NewSystemConfigRepository(dbManager *database.Manager) SystemConfigRepository {
	return &systemConfigRepository{dbManager: dbManager}
}

func (r *systemConfigRepository) ListAll(ctx context.Context) ([]*model.SystemConfig, error) {
	db, err := r.dbManager.GetMetaReadDB()
	if err != nil {
		return nil, fmt.Errorf("system_config ListAll: get meta db: %w", err)
	}
	var rows []*model.SystemConfig
	if err := db.WithContext(ctx).Order("config_key ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("system_config ListAll: %w", err)
	}
	return rows, nil
}

func (r *systemConfigRepository) GetByKey(ctx context.Context, key string) (*model.SystemConfig, error) {
	db, err := r.dbManager.GetMetaReadDB()
	if err != nil {
		return nil, fmt.Errorf("system_config GetByKey: get meta db: %w", err)
	}
	var cfg model.SystemConfig
	if err := db.WithContext(ctx).Where("config_key = ?", key).First(&cfg).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("system_config GetByKey: %w", err)
	}
	return &cfg, nil
}

func (r *systemConfigRepository) Upsert(ctx context.Context, cfg *model.SystemConfig) error {
	if cfg == nil || cfg.ConfigKey == "" {
		return fmt.Errorf("config_key required")
	}
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return fmt.Errorf("system_config Upsert: get meta db: %w", err)
	}
	// 只覆盖明确传值的字段（description / value_type 空时保留原值要靠 admin 接口层判断）
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "config_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"value_json", "value_type", "description", "updated_by",
		}),
	}).Create(cfg).Error
}

func (r *systemConfigRepository) Delete(ctx context.Context, key string) (bool, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return false, fmt.Errorf("system_config Delete: get meta db: %w", err)
	}
	result := db.WithContext(ctx).Where("config_key = ?", key).Delete(&model.SystemConfig{})
	if result.Error != nil {
		return false, fmt.Errorf("system_config Delete: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

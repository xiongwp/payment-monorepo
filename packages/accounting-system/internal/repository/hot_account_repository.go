package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
)

// HotAccountRepository 热点账户配置仓储（存于 account_meta）
type HotAccountRepository interface {
	// LoadEnabledAccounts 加载所有 enabled=1 的热点账户号列表（启动时 + 重载时调用）
	LoadEnabledAccounts(ctx context.Context) ([]string, error)

	// ListAll 返回全量配置（包含 disabled），供管理接口查看
	ListAll(ctx context.Context) ([]*model.HotAccountConfig, error)

	// Create 新增热点账户配置
	Create(ctx context.Context, cfg *model.HotAccountConfig) error

	// Update 更新热点账户配置（enabled / description）
	Update(ctx context.Context, id int64, enabled bool, description string) error

	// Delete 删除热点账户配置
	Delete(ctx context.Context, id int64) error
}

type hotAccountRepository struct {
	dbManager *database.Manager
}

// NewHotAccountRepository 创建热点账户仓储
func NewHotAccountRepository(dbManager *database.Manager) HotAccountRepository {
	return &hotAccountRepository{dbManager: dbManager}
}

func (r *hotAccountRepository) LoadEnabledAccounts(ctx context.Context) ([]string, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, err
	}
	var accounts []string
	qErr := db.WithContext(ctx).
		Model(&model.HotAccountConfig{}).
		Where("enabled = 1").
		Pluck("account_no", &accounts).Error
	return accounts, qErr
}

func (r *hotAccountRepository) ListAll(ctx context.Context) ([]*model.HotAccountConfig, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, err
	}
	var configs []*model.HotAccountConfig
	qErr := db.WithContext(ctx).Order("id ASC").Find(&configs).Error
	return configs, qErr
}

func (r *hotAccountRepository) Create(ctx context.Context, cfg *model.HotAccountConfig) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return err
	}
	now := time.Now()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	return db.WithContext(ctx).Create(cfg).Error
}

func (r *hotAccountRepository) Update(ctx context.Context, id int64, enabled bool, description string) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return err
	}
	result := db.WithContext(ctx).
		Model(&model.HotAccountConfig{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"enabled":     enabled,
			"description": description,
			"updated_at":  time.Now(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("hot_account_config id=%d not found", id)
	}
	return nil
}

func (r *hotAccountRepository) Delete(ctx context.Context, id int64) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return err
	}
	result := db.WithContext(ctx).
		Where("id = ?", id).
		Delete(&model.HotAccountConfig{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("hot_account_config id=%d not found", id)
	}
	return nil
}

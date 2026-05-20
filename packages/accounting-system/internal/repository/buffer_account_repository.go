package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
)

// BufferAccountRepository 缓冲记账账户配置仓储（存于 account_meta）
type BufferAccountRepository interface {
	// LoadEnabledByLevel 加载指定刷新间隔等级下所有 enabled=1 的账户号列表
	LoadEnabledByLevel(ctx context.Context, level model.BufferFlushLevel) ([]string, error)

	// LoadAllEnabled 加载所有 enabled=1 的配置（包含所有等级）
	LoadAllEnabled(ctx context.Context) ([]*model.BufferAccountConfig, error)

	// ListAll 返回全量配置（含 disabled），供管理接口查看
	ListAll(ctx context.Context) ([]*model.BufferAccountConfig, error)

	// Create 新增缓冲记账账户配置
	Create(ctx context.Context, cfg *model.BufferAccountConfig) error

	// Update 更新缓冲记账账户配置（enabled / flush_interval_level / description）
	Update(ctx context.Context, id int64, enabled bool, level model.BufferFlushLevel, description string) error

	// Delete 删除缓冲记账账户配置
	Delete(ctx context.Context, id int64) error
}

type bufferAccountRepository struct {
	dbManager *database.Manager
}

// NewBufferAccountRepository 创建缓冲记账账户仓储
func NewBufferAccountRepository(dbManager *database.Manager) BufferAccountRepository {
	return &bufferAccountRepository{dbManager: dbManager}
}

func (r *bufferAccountRepository) LoadEnabledByLevel(ctx context.Context, level model.BufferFlushLevel) ([]string, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, err
	}
	var accounts []string
	qErr := db.WithContext(ctx).
		Model(&model.BufferAccountConfig{}).
		Where("enabled = 1 AND flush_interval_level = ?", level).
		Pluck("account_no", &accounts).Error
	return accounts, qErr
}

func (r *bufferAccountRepository) LoadAllEnabled(ctx context.Context) ([]*model.BufferAccountConfig, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, err
	}
	var configs []*model.BufferAccountConfig
	qErr := db.WithContext(ctx).
		Where("enabled = 1").
		Order("flush_interval_level ASC, id ASC").
		Find(&configs).Error
	return configs, qErr
}

func (r *bufferAccountRepository) ListAll(ctx context.Context) ([]*model.BufferAccountConfig, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, err
	}
	var configs []*model.BufferAccountConfig
	qErr := db.WithContext(ctx).
		Order("flush_interval_level ASC, id ASC").
		Find(&configs).Error
	return configs, qErr
}

func (r *bufferAccountRepository) Create(ctx context.Context, cfg *model.BufferAccountConfig) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return err
	}
	now := time.Now()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now
	return db.WithContext(ctx).Create(cfg).Error
}

func (r *bufferAccountRepository) Update(ctx context.Context, id int64, enabled bool, level model.BufferFlushLevel, description string) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return err
	}
	result := db.WithContext(ctx).
		Model(&model.BufferAccountConfig{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"enabled":              enabled,
			"flush_interval_level": level,
			"description":          description,
			"updated_at":           time.Now(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("buffer_account_config id=%d not found", id)
	}
	return nil
}

func (r *bufferAccountRepository) Delete(ctx context.Context, id int64) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return err
	}
	result := db.WithContext(ctx).
		Where("id = ?", id).
		Delete(&model.BufferAccountConfig{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("buffer_account_config id=%d not found", id)
	}
	return nil
}

package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
)

// ServiceInstanceRepository 管理服务实例注册表（account_meta.service_instance）。
type ServiceInstanceRepository interface {
	// Register 注册或更新本实例信息（upsert）。
	Register(ctx context.Context, inst *model.ServiceInstance) error
	// Heartbeat 更新 last_heartbeat，保持实例为活跃状态。
	Heartbeat(ctx context.Context, instanceID string) error
	// Deregister 将实例状态置为 stopped。
	Deregister(ctx context.Context, instanceID string) error
	// ListAlive 返回在 aliveThreshold 内有心跳且 status=alive 的所有实例。
	ListAlive(ctx context.Context, aliveThreshold time.Duration) ([]*model.ServiceInstance, error)
}

type serviceInstanceRepository struct {
	dbManager *database.Manager
}

func NewServiceInstanceRepository(dbManager *database.Manager) ServiceInstanceRepository {
	return &serviceInstanceRepository{dbManager: dbManager}
}

func (r *serviceInstanceRepository) Register(ctx context.Context, inst *model.ServiceInstance) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return fmt.Errorf("service_instance Register: get meta db: %w", err)
	}
	return db.WithContext(ctx).Table("service_instance").Save(inst).Error
}

func (r *serviceInstanceRepository) Heartbeat(ctx context.Context, instanceID string) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return fmt.Errorf("service_instance Heartbeat: get meta db: %w", err)
	}
	return db.WithContext(ctx).
		Table("service_instance").
		Where("instance_id = ?", instanceID).
		Updates(map[string]interface{}{
			"last_heartbeat": time.Now(),
			"status":         model.ServiceInstanceStatusAlive,
		}).Error
}

func (r *serviceInstanceRepository) Deregister(ctx context.Context, instanceID string) error {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return fmt.Errorf("service_instance Deregister: get meta db: %w", err)
	}
	return db.WithContext(ctx).
		Table("service_instance").
		Where("instance_id = ?", instanceID).
		Update("status", model.ServiceInstanceStatusStopped).Error
}

func (r *serviceInstanceRepository) ListAlive(ctx context.Context, aliveThreshold time.Duration) ([]*model.ServiceInstance, error) {
	db, err := r.dbManager.GetMetaDB()
	if err != nil {
		return nil, fmt.Errorf("service_instance ListAlive: get meta db: %w", err)
	}
	cutoff := time.Now().Add(-aliveThreshold)
	var instances []*model.ServiceInstance
	if err := db.WithContext(ctx).
		Table("service_instance").
		Where("status = ? AND last_heartbeat >= ?", model.ServiceInstanceStatusAlive, cutoff).
		Find(&instances).Error; err != nil {
		return nil, fmt.Errorf("service_instance ListAlive: %w", err)
	}
	return instances, nil
}

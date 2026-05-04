package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// BatchOrderRepository 原子批量记账订单仓储接口
//
// 路由规则：按 business_no 末位数字路由（RouteByNumericStr），与分库分表架构一致。
// 幂等设计：CreateIfNotExists 重复创建时读回已有记录，调用方通过 order.Status 判断是否需要执行。
type BatchOrderRepository interface {
	// CreateIfNotExists creates the batch order.
	// If batch_id already exists (duplicate key), reads the existing record into order.
	CreateIfNotExists(ctx context.Context, order *model.BatchOrder) error
	// GetByBatchID retrieves a batch order; businessNo is required for routing. Returns nil if not found.
	GetByBatchID(ctx context.Context, batchID, businessNo string) (*model.BatchOrder, error)
	// UpdateStatus updates status and optionally errorMsg.
	UpdateStatus(ctx context.Context, batchID, businessNo string, status int8, errorMsg string) error
}

type batchOrderRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewBatchOrderRepository 创建批量订单仓储
func NewBatchOrderRepository(dbManager *database.Manager, router *sharding.Router) BatchOrderRepository {
	return &batchOrderRepository{dbManager: dbManager, router: router}
}

func (r *batchOrderRepository) route(ctx context.Context, businessNo string) (*gorm.DB, string, error) {
	dbIndex, tableIndex := r.router.RouteByNumericStr(businessNo)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, "", fmt.Errorf("batch_order route(%s): get db[%d]: %w", businessNo, dbIndex, err)
	}
	return db.WithContext(ctx), r.router.GetTableName("batch_order", tableIndex), nil
}

func isDuplicateKeyBatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}

func (r *batchOrderRepository) CreateIfNotExists(ctx context.Context, order *model.BatchOrder) error {
	db, tableName, err := r.route(ctx, order.BusinessNo)
	if err != nil {
		return err
	}

	result := db.Table(tableName).Create(order)
	if result.Error != nil {
		if isDuplicateKeyBatch(result.Error) {
			// Idempotent: read back the existing record so caller can inspect Status and Extra.
			return db.Table(tableName).Where("batch_id = ?", order.BatchID).First(order).Error
		}
		return result.Error
	}
	return nil
}

func (r *batchOrderRepository) GetByBatchID(ctx context.Context, batchID, businessNo string) (*model.BatchOrder, error) {
	db, tableName, err := r.route(ctx, businessNo)
	if err != nil {
		return nil, err
	}

	var order model.BatchOrder
	if err := db.Table(tableName).Where("batch_id = ?", batchID).First(&order).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("batch_order GetByBatchID: %w", err)
	}
	return &order, nil
}

func (r *batchOrderRepository) UpdateStatus(ctx context.Context, batchID, businessNo string, status int8, errorMsg string) error {
	db, tableName, err := r.route(ctx, businessNo)
	if err != nil {
		return err
	}

	updates := map[string]interface{}{
		"status":     status,
		"updated_at": time.Now(),
	}
	if errorMsg != "" {
		updates["error_message"] = errorMsg
	}

	return db.Table(tableName).Where("batch_id = ?", batchID).Updates(updates).Error
}

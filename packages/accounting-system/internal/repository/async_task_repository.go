package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"gorm.io/gorm"
)

// AsyncTaskRepository 异步任务仓储接口
type AsyncTaskRepository interface {
	// Create inserts a new async task. Routes by task.BusinessNo.
	Create(ctx context.Context, task *model.AsyncTask) error
	// FindPendingByShard returns pending/failed tasks for a specific shard.
	FindPendingByShard(ctx context.Context, dbIndex, tableIndex int, now time.Time, limit int) ([]*model.AsyncTask, error)
	// ClaimProcessing CAS from pending/failed to processing.
	// Returns true if this caller successfully claimed the task.
	ClaimProcessing(ctx context.Context, task *model.AsyncTask) (bool, error)
	// UpdateSuccess marks a task as successfully completed.
	UpdateSuccess(ctx context.Context, task *model.AsyncTask) error
	// UpdateFailed marks a task as failed with an error message.
	UpdateFailed(ctx context.Context, task *model.AsyncTask, errorMsg string) error
	// MarkPendingManual marks a task as requiring manual intervention (max retries exhausted).
	MarkPendingManual(ctx context.Context, task *model.AsyncTask, errorMsg string) error
	// ScheduleRetry schedules a task for retry using exponential backoff (1<<retryCount minutes).
	ScheduleRetry(ctx context.Context, task *model.AsyncTask, errorMsg string) error
	// FindByTaskID finds a task by taskID scanning all shards.
	// Returns nil if not found.
	FindByTaskID(ctx context.Context, taskID string) (*model.AsyncTask, error)
	// FindPendingManualByShard returns tasks awaiting manual processing for a specific shard.
	FindPendingManualByShard(ctx context.Context, dbIndex, tableIndex, limit int) ([]*model.AsyncTask, error)
	// RecoverStuckProcessing resets tasks stuck in PROCESSING longer than stuckThreshold back to FAILED
	// so they are picked up again on the next ProcessPendingTasks scan.
	// Returns the total number of tasks reset across all shards.
	// Safe to call concurrently; uses a single UPDATE per shard (no locks held).
	RecoverStuckProcessing(ctx context.Context, stuckThreshold time.Duration) (int64, error)
}

type asyncTaskRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewAsyncTaskRepository 创建异步任务仓储
func NewAsyncTaskRepository(dbManager *database.Manager, router *sharding.Router) AsyncTaskRepository {
	return &asyncTaskRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// Create inserts a new async task. Routes by task.BusinessNo.
func (r *asyncTaskRepository) Create(ctx context.Context, task *model.AsyncTask) error {
	dbIndex, tableIndex := r.router.RouteByNumericStr(task.BusinessNo)
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("async_task Create: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).Create(task).Error
}

// FindPendingByShard returns pending/failed tasks for a specific shard whose next_retry_time <= now.
func (r *asyncTaskRepository) FindPendingByShard(ctx context.Context, dbIndex, tableIndex int, now time.Time, limit int) ([]*model.AsyncTask, error) {
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("async_task FindPendingByShard: get db[%d]: %w", dbIndex, err)
	}

	var tasks []*model.AsyncTask
	err = db.WithContext(ctx).Table(tableName).
		Where("status IN (?) AND (next_retry_time IS NULL OR next_retry_time <= ?)",
			[]int8{model.AsyncTaskStatusPending, model.AsyncTaskStatusFailed}, now).
		Order("next_retry_time ASC").
		Limit(limit).
		Find(&tasks).Error
	if err != nil {
		return nil, fmt.Errorf("async_task FindPendingByShard: %w", err)
	}
	return tasks, nil
}

// ClaimProcessing CAS from pending/failed to processing.
// Returns true if this caller successfully claimed the task.
func (r *asyncTaskRepository) ClaimProcessing(ctx context.Context, task *model.AsyncTask) (bool, error) {
	dbIndex, tableIndex := r.router.RouteByNumericStr(task.BusinessNo)
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return false, fmt.Errorf("async_task ClaimProcessing: get db[%d]: %w", dbIndex, err)
	}

	result := db.WithContext(ctx).Table(tableName).
		Where("task_id = ? AND status IN (?)",
			task.TaskID,
			[]int8{model.AsyncTaskStatusPending, model.AsyncTaskStatusFailed},
		).
		Updates(map[string]interface{}{
			"status":     model.AsyncTaskStatusProcessing,
			"updated_at": time.Now(),
		})
	if result.Error != nil {
		return false, fmt.Errorf("async_task ClaimProcessing: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// UpdateSuccess marks a task as successfully completed.
func (r *asyncTaskRepository) UpdateSuccess(ctx context.Context, task *model.AsyncTask) error {
	dbIndex, tableIndex := r.router.RouteByNumericStr(task.BusinessNo)
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("async_task UpdateSuccess: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]interface{}{
			"status":     model.AsyncTaskStatusSuccess,
			"updated_at": time.Now(),
		}).Error
}

// UpdateFailed marks a task as failed with an error message.
func (r *asyncTaskRepository) UpdateFailed(ctx context.Context, task *model.AsyncTask, errorMsg string) error {
	dbIndex, tableIndex := r.router.RouteByNumericStr(task.BusinessNo)
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("async_task UpdateFailed: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]interface{}{
			"status":        model.AsyncTaskStatusFailed,
			"error_message": errorMsg,
			"updated_at":    time.Now(),
		}).Error
}

// ScheduleRetry schedules a task for retry using exponential backoff (1<<retryCount minutes).
func (r *asyncTaskRepository) ScheduleRetry(ctx context.Context, task *model.AsyncTask, errorMsg string) error {
	dbIndex, tableIndex := r.router.RouteByNumericStr(task.BusinessNo)
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("async_task ScheduleRetry: get db[%d]: %w", dbIndex, err)
	}

	backoff := time.Duration(1<<task.RetryCount) * time.Minute
	nextRetry := time.Now().Add(backoff)

	return db.WithContext(ctx).Table(tableName).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]interface{}{
			"status":          model.AsyncTaskStatusFailed,
			"retry_count":     gorm.Expr("retry_count + 1"),
			"next_retry_time": nextRetry,
			"error_message":   errorMsg,
			"updated_at":      time.Now(),
		}).Error
}

// MarkPendingManual marks a task as requiring manual intervention.
func (r *asyncTaskRepository) MarkPendingManual(ctx context.Context, task *model.AsyncTask, errorMsg string) error {
	dbIndex, tableIndex := r.router.RouteByNumericStr(task.BusinessNo)
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("async_task MarkPendingManual: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Where("task_id = ?", task.TaskID).
		Updates(map[string]interface{}{
			"status":        model.AsyncTaskStatusPendingManual,
			"error_message": errorMsg,
			"updated_at":    time.Now(),
		}).Error
}

// FindPendingManualByShard returns tasks awaiting manual processing for a specific shard.
func (r *asyncTaskRepository) FindPendingManualByShard(ctx context.Context, dbIndex, tableIndex, limit int) ([]*model.AsyncTask, error) {
	tableName := fmt.Sprintf("async_task_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("async_task FindPendingManualByShard: get db[%d]: %w", dbIndex, err)
	}

	var tasks []*model.AsyncTask
	err = db.WithContext(ctx).Table(tableName).
		Where("status = ?", model.AsyncTaskStatusPendingManual).
		Order("created_at ASC").
		Limit(limit).
		Find(&tasks).Error
	if err != nil {
		return nil, fmt.Errorf("async_task FindPendingManualByShard: %w", err)
	}
	return tasks, nil
}

// RecoverStuckProcessing resets async tasks stuck in PROCESSING beyond stuckThreshold back to
// FAILED so they are eligible for retry on the next ProcessPendingTasks scan.
//
// This handles the server-crash scenario: a task claimed to PROCESSING but never completed
// because the process died mid-execution. Resetting to FAILED (not PENDING) ensures the
// retry_count is preserved and exponential backoff still applies.
func (r *asyncTaskRepository) RecoverStuckProcessing(ctx context.Context, stuckThreshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-stuckThreshold)
	var total int64
	for _, shard := range r.router.GetAllShards() {
		tableName := fmt.Sprintf("async_task_%02d", shard.TableIndex)

		db, err := r.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return total, fmt.Errorf("async_task RecoverStuckProcessing: get db[%d]: %w", shard.DBIndex, err)
		}

		result := db.WithContext(ctx).Table(tableName).
			Where("status = ? AND updated_at < ?", model.AsyncTaskStatusProcessing, cutoff).
			Updates(map[string]interface{}{
				"status":        model.AsyncTaskStatusFailed,
				"error_message": "recovered: task stuck in PROCESSING (server restart or crash)",
				"updated_at":    time.Now(),
			})
		if result.Error != nil {
			return total, fmt.Errorf("async_task RecoverStuckProcessing: shard(%d,%d): %w",
				shard.DBIndex, shard.TableIndex, result.Error)
		}
		total += result.RowsAffected
	}
	return total, nil
}

// FindByTaskID finds a task by taskID scanning all shards sequentially.
// Returns nil if not found.
func (r *asyncTaskRepository) FindByTaskID(ctx context.Context, taskID string) (*model.AsyncTask, error) {
	for _, shard := range r.router.GetAllShards() {
		tableName := fmt.Sprintf("async_task_%02d", shard.TableIndex)

		db, err := r.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return nil, fmt.Errorf("async_task FindByTaskID: get db[%d]: %w", shard.DBIndex, err)
		}

		var task model.AsyncTask
		result := db.WithContext(ctx).Table(tableName).
			Where("task_id = ?", taskID).
			First(&task)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				continue
			}
			return nil, fmt.Errorf("async_task FindByTaskID: db[%d] table %s: %w", shard.DBIndex, tableName, result.Error)
		}
		return &task, nil
	}
	return nil, nil
}

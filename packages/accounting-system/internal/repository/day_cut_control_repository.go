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
	"gorm.io/gorm/clause"
)

// DayCutDateStatusRow represents one (cut_date, run_id, currency, status, count) aggregate row.
type DayCutDateStatusRow struct {
	CutDate  string `gorm:"column:cut_date"`
	RunID    int    `gorm:"column:run_id"`
	Currency string `gorm:"column:currency"`
	Status   int8   `gorm:"column:status"`
	Count    int    `gorm:"column:count"`
}

// DayCutControlRepository 日切控制仓储接口
type DayCutControlRepository interface {
	// Upsert initializes a day-cut control record; idempotent on (database_index, table_index, cut_date, run_id).
	// On conflict, only updates status back to pending (supports retrigger).
	Upsert(ctx context.Context, dbIndex, tableIndex int, control *model.DayCutControl) error
	// UpdateStatus updates the status and optionally start_time of a control record.
	UpdateStatus(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, status int8, errorMsg string, startTime *time.Time) error
	// UpdateStatusCompleted marks the day-cut as completed, recording the last transaction ID.
	UpdateStatusCompleted(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, startTime *time.Time, txID string) error
	// SetLastProcessedID 推进断点续跑游标。每个 chunk 处理完后调用，记录本 run
	// 已处理到的最大 account_transaction.id（含）。用 WHERE last_processed_id < ?
	// 单调递增保护，防止重排乱序的 update 把游标回退。
	SetLastProcessedID(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, lastID uint64) error
	// SetLastProcessedIDTx 同 SetLastProcessedID 但在给定事务上执行。
	// 原子 chunk commit 用：与 UpsertBatchIncrementalTx 在同一个 tx 内，避免崩溃后重复累加。
	SetLastProcessedIDTx(tx *gorm.DB, dbIndex, tableIndex int, cutDate string, runID int, lastID uint64) error
	// GetByShard returns the day-cut control record for the given shard, date, and run.
	// Returns nil (no error) if no record exists yet.
	GetByShard(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int) (*model.DayCutControl, error)
	// QueryStatusSummary returns a map[status]count for the given shard and date (all runs).
	QueryStatusSummary(ctx context.Context, dbIndex, tableIndex int, cutDate string) (map[int8]int, error)
	// ListAllStatuses returns all (cut_date, run_id, status, count) rows for the given shard,
	// ordered by cut_date DESC, run_id DESC.
	ListAllStatuses(ctx context.Context, dbIndex, tableIndex int) ([]DayCutDateStatusRow, error)
	// GetMaxRunID returns the highest run_id for a given shard+date. Returns 0 if no records exist.
	GetMaxRunID(ctx context.Context, dbIndex, tableIndex int, cutDate string) (int, error)
	// ListStuckProcessing 跨所有 (cutDate, runID) 列出本分片处于 PROCESSING 且
	// updated_at 早于 olderThan 的记录。用于服务启动时 / watchdog 周期检测，
	// 把这些"无 worker 跑"的卡死分片重新派发执行（DB 断网 + 重连场景）。
	ListStuckProcessing(ctx context.Context, dbIndex, tableIndex int, olderThan time.Time) ([]*model.DayCutControl, error)
	// GetLatestCompletedRunID scans all shards and returns the highest run_id for cutDate
	// where ALL expected shards have status=COMPLETED.
	// Returns found=false if no such fully-completed run exists.
	GetLatestCompletedRunID(ctx context.Context, cutDate string) (runID int, found bool, err error)
}

type dayCutControlRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewDayCutControlRepository 创建日切控制仓储
func NewDayCutControlRepository(dbManager *database.Manager, router *sharding.Router) DayCutControlRepository {
	return &dayCutControlRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// statusCount is a helper struct for QueryStatusSummary aggregation.
type statusCount struct {
	Status int8 `gorm:"column:status"`
	Count  int  `gorm:"column:count"`
}

// Upsert initializes a day-cut control record; idempotent on (database_index, table_index, cut_date, run_id).
func (r *dayCutControlRepository) Upsert(ctx context.Context, dbIndex, tableIndex int, control *model.DayCutControl) error {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("day_cut_control Upsert: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "database_index"},
				{Name: "table_index"},
				{Name: "cut_date"},
				{Name: "run_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{"status"}),
		}).
		Create(control).Error
}

// UpdateStatus updates the status and optionally start_time of a control record.
func (r *dayCutControlRepository) UpdateStatus(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, status int8, errorMsg string, startTime *time.Time) error {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("day_cut_control UpdateStatus: get db[%d]: %w", dbIndex, err)
	}

	updates := map[string]interface{}{
		"status":        status,
		"error_message": errorMsg,
		"updated_at":    time.Now(),
	}
	if startTime != nil {
		updates["start_time"] = startTime
	}
	if status == model.DayCutStatusCompleted {
		now := time.Now()
		updates["end_time"] = &now
	}

	return db.WithContext(ctx).Table(tableName).
		Where("database_index = ? AND table_index = ? AND cut_date = ? AND run_id = ?", dbIndex, tableIndex, cutDate, runID).
		Updates(updates).Error
}

// GetByShard returns the day-cut control record for the given shard, date, and run.
func (r *dayCutControlRepository) GetByShard(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int) (*model.DayCutControl, error) {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("day_cut_control GetByShard: get db[%d]: %w", dbIndex, err)
	}

	var control model.DayCutControl
	result := db.WithContext(ctx).Table(tableName).
		Where("database_index = ? AND table_index = ? AND cut_date = ? AND run_id = ?", dbIndex, tableIndex, cutDate, runID).
		First(&control)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("day_cut_control GetByShard: %w", result.Error)
	}
	return &control, nil
}

// UpdateStatusCompleted marks the day-cut as completed, recording the last transaction ID.
func (r *dayCutControlRepository) UpdateStatusCompleted(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, startTime *time.Time, txID string) error {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("day_cut_control UpdateStatusCompleted: get db[%d]: %w", dbIndex, err)
	}

	now := time.Now()
	updates := map[string]interface{}{
		"status":              model.DayCutStatusCompleted,
		"error_message":       "",
		"last_transaction_id": txID,
		"cut_time":            &now,
		"end_time":            &now,
		"updated_at":          now,
	}
	if startTime != nil {
		updates["start_time"] = startTime
	}

	return db.WithContext(ctx).Table(tableName).
		Where("database_index = ? AND table_index = ? AND cut_date = ? AND run_id = ?", dbIndex, tableIndex, cutDate, runID).
		Updates(updates).Error
}

// SetLastProcessedID 推进断点续跑游标。
// 用 WHERE last_processed_id < ? 保护，避免 race 下重复 update 把游标回退。
// 同一 run 内只能向前推进，永不回退；崩溃恢复时直接读取 last_processed_id 即可继续。
func (r *dayCutControlRepository) SetLastProcessedID(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, lastID uint64) error {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("day_cut_control SetLastProcessedID: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Where("database_index = ? AND table_index = ? AND cut_date = ? AND run_id = ? AND last_processed_id < ?",
			dbIndex, tableIndex, cutDate, runID, lastID).
		Updates(map[string]interface{}{
			"last_processed_id": lastID,
			"updated_at":        time.Now(),
		}).Error
}

// SetLastProcessedIDTx 同 SetLastProcessedID 但在给定事务上执行（原子 chunk commit 用）。
func (r *dayCutControlRepository) SetLastProcessedIDTx(tx *gorm.DB, dbIndex, tableIndex int, cutDate string, runID int, lastID uint64) error {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)
	return tx.Table(tableName).
		Where("database_index = ? AND table_index = ? AND cut_date = ? AND run_id = ? AND last_processed_id < ?",
			dbIndex, tableIndex, cutDate, runID, lastID).
		Updates(map[string]interface{}{
			"last_processed_id": lastID,
			"updated_at":        time.Now(),
		}).Error
}

// ListStuckProcessing 见接口注释。返回本分片所有 PROCESSING + updated_at < olderThan 的行。
func (r *dayCutControlRepository) ListStuckProcessing(ctx context.Context, dbIndex, tableIndex int, olderThan time.Time) ([]*model.DayCutControl, error) {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)
	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("day_cut_control ListStuckProcessing: get db[%d]: %w", dbIndex, err)
	}
	var rows []*model.DayCutControl
	err = db.WithContext(ctx).Table(tableName).
		Where("database_index = ? AND table_index = ? AND status = ? AND updated_at < ?",
			dbIndex, tableIndex, model.DayCutStatusProcessing, olderThan).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("day_cut_control ListStuckProcessing db[%d] table[%d]: %w", dbIndex, tableIndex, err)
	}
	return rows, nil
}

// QueryStatusSummary returns a map[status]count for the given shard and date (all runs combined).
func (r *dayCutControlRepository) QueryStatusSummary(ctx context.Context, dbIndex, tableIndex int, cutDate string) (map[int8]int, error) {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("day_cut_control QueryStatusSummary: get db[%d]: %w", dbIndex, err)
	}

	var rows []statusCount
	err = db.WithContext(ctx).Table(tableName).
		Select("status, COUNT(*) as count").
		Where("cut_date = ?", cutDate).
		Group("status").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("day_cut_control QueryStatusSummary: %w", err)
	}

	result := make(map[int8]int, len(rows))
	for _, row := range rows {
		result[row.Status] = row.Count
	}
	return result, nil
}

// ListAllStatuses returns all (cut_date, run_id, status, count) rows for the given shard,
// ordered by cut_date DESC, run_id DESC.
func (r *dayCutControlRepository) ListAllStatuses(ctx context.Context, dbIndex, tableIndex int) ([]DayCutDateStatusRow, error) {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return nil, fmt.Errorf("day_cut_control ListAllStatuses: get db[%d]: %w", dbIndex, err)
	}

	var rows []DayCutDateStatusRow
	err = db.WithContext(ctx).Table(tableName).
		Select("DATE_FORMAT(cut_date, '%Y-%m-%d') as cut_date, run_id, currency, status, COUNT(*) as count").
		Group("cut_date, run_id, currency, status").
		Order("cut_date DESC, run_id DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("day_cut_control ListAllStatuses: %w", err)
	}
	return rows, nil
}

// GetMaxRunID returns the highest run_id for the given shard+date. Returns 0 if no records exist.
func (r *dayCutControlRepository) GetMaxRunID(ctx context.Context, dbIndex, tableIndex int, cutDate string) (int, error) {
	tableName := r.router.GetTableName("day_cut_control", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return 0, fmt.Errorf("day_cut_control GetMaxRunID: get db[%d]: %w", dbIndex, err)
	}

	var maxRunID int
	err = db.WithContext(ctx).Table(tableName).
		Select("COALESCE(MAX(run_id), 0)").
		Where("cut_date = ?", cutDate).
		Scan(&maxRunID).Error
	if err != nil {
		return 0, fmt.Errorf("day_cut_control GetMaxRunID: db[%d] table[%d]: %w", dbIndex, tableIndex, err)
	}
	return maxRunID, nil
}

// runShardStatus is a helper for GetLatestCompletedRunID aggregation.
type runShardStatus struct {
	RunID  int  `gorm:"column:run_id"`
	Status int8 `gorm:"column:status"`
	Count  int  `gorm:"column:count"`
}

// GetLatestCompletedRunID scans all shards and returns the highest run_id for cutDate
// where ALL shards have status=COMPLETED.
func (r *dayCutControlRepository) GetLatestCompletedRunID(ctx context.Context, cutDate string) (int, bool, error) {
	type runStats struct {
		completed int
		total     int
	}
	runMap := make(map[int]*runStats)

	allShards := r.router.GetAllShards()
	for _, shard := range allShards {
		tableName := r.router.GetTableName("day_cut_control", shard.TableIndex)
		db, err := r.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return 0, false, fmt.Errorf("GetLatestCompletedRunID: get db[%d]: %w", shard.DBIndex, err)
		}

		var rows []runShardStatus
		err = db.WithContext(ctx).Table(tableName).
			Select("run_id, status, COUNT(*) as count").
			Where("cut_date = ?", cutDate).
			Group("run_id, status").
			Scan(&rows).Error
		if err != nil {
			return 0, false, fmt.Errorf("GetLatestCompletedRunID: db[%d] table[%d]: %w", shard.DBIndex, shard.TableIndex, err)
		}

		for _, row := range rows {
			if _, ok := runMap[row.RunID]; !ok {
				runMap[row.RunID] = &runStats{}
			}
			runMap[row.RunID].total += row.Count
			if row.Status == model.DayCutStatusCompleted {
				runMap[row.RunID].completed += row.Count
			}
		}
	}

	totalShards := len(allShards)
	maxRunID := 0
	for runID, stats := range runMap {
		if stats.completed == totalShards && runID > maxRunID {
			maxRunID = runID
		}
	}

	if maxRunID == 0 {
		return 0, false, nil
	}
	return maxRunID, true, nil
}


package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
)

// DistributedLockRepository 分布式锁仓储接口
type DistributedLockRepository interface {
	// TryAcquire attempts to INSERT a lock record. Returns true if acquired.
	// If existing lock is expired, deletes it first then retries.
	TryAcquire(ctx context.Context, lockKey, lockValue string, ttl time.Duration, dbIndex, tableIndex int) (bool, error)
	// Release deletes the lock only if lockValue matches (prevents releasing other holders' locks).
	Release(ctx context.Context, lockKey, lockValue string, dbIndex, tableIndex int) error
	// CleanExpired removes all expired lock records across all shards.
	CleanExpired(ctx context.Context) (int64, error)
}

type distributedLockRepository struct {
	dbManager *database.Manager
	router    *sharding.Router
}

// NewDistributedLockRepository 创建分布式锁仓储
func NewDistributedLockRepository(dbManager *database.Manager, router *sharding.Router) DistributedLockRepository {
	return &distributedLockRepository{
		dbManager: dbManager,
		router:    router,
	}
}

// isDuplicateKeyLock detects MySQL 1062 duplicate key errors for the distributed lock table.
func isDuplicateKeyLock(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Duplicate entry")
}

// TryAcquire attempts to acquire a distributed lock.
// 1. Delete any expired lock with the same key.
// 2. Attempt to INSERT the new lock record.
// 3. If duplicate key error → return false, nil (lock already held).
// 4. On success → return true, nil.
func (r *distributedLockRepository) TryAcquire(ctx context.Context, lockKey, lockValue string, ttl time.Duration, dbIndex, tableIndex int) (bool, error) {
	tableName := fmt.Sprintf("distributed_lock_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return false, fmt.Errorf("distributed_lock TryAcquire: get db[%d]: %w", dbIndex, err)
	}

	// Step 1: Delete any expired lock with the same key.
	if err := db.WithContext(ctx).Table(tableName).
		Where("lock_key = ? AND expire_time < NOW()", lockKey).
		Delete(&model.DistributedLock{}).Error; err != nil {
		return false, fmt.Errorf("distributed_lock TryAcquire: clean expired: %w", err)
	}

	// Step 2: Attempt to INSERT the new lock record.
	lock := &model.DistributedLock{
		LockKey:    lockKey,
		LockValue:  lockValue,
		ExpireTime: time.Now().Add(ttl),
	}
	result := db.WithContext(ctx).Table(tableName).Create(lock)
	if result.Error != nil {
		// Step 3: Duplicate key → lock already held by another holder.
		if isDuplicateKeyLock(result.Error) {
			return false, nil
		}
		return false, fmt.Errorf("distributed_lock TryAcquire: insert: %w", result.Error)
	}

	// Step 4: Successfully acquired.
	return true, nil
}

// Release deletes the lock only if lockValue matches.
func (r *distributedLockRepository) Release(ctx context.Context, lockKey, lockValue string, dbIndex, tableIndex int) error {
	tableName := fmt.Sprintf("distributed_lock_%02d", tableIndex)

	db, err := r.dbManager.GetDB(dbIndex)
	if err != nil {
		return fmt.Errorf("distributed_lock Release: get db[%d]: %w", dbIndex, err)
	}

	return db.WithContext(ctx).Table(tableName).
		Where("lock_key = ? AND lock_value = ?", lockKey, lockValue).
		Delete(&model.DistributedLock{}).Error
}

// CleanExpired removes all expired lock records across all shards.
func (r *distributedLockRepository) CleanExpired(ctx context.Context) (int64, error) {
	var totalAffected int64

	for _, shard := range r.router.GetAllShards() {
		tableName := fmt.Sprintf("distributed_lock_%02d", shard.TableIndex)

		db, err := r.dbManager.GetDB(shard.DBIndex)
		if err != nil {
			return totalAffected, fmt.Errorf("distributed_lock CleanExpired: get db[%d]: %w", shard.DBIndex, err)
		}

		result := db.WithContext(ctx).Table(tableName).
			Where("expire_time < NOW()").
			Delete(&model.DistributedLock{})
		if result.Error != nil {
			return totalAffected, fmt.Errorf("distributed_lock CleanExpired: db[%d] table %s: %w",
				shard.DBIndex, tableName, result.Error)
		}
		totalAffected += result.RowsAffected
	}

	return totalAffected, nil
}

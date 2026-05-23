package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
)

// RotationLockManager 实现 service.LockManager（基于既有 distributed_lock 表）。
//
// 锁 key 格式："rotation:la:{logical_account_id}"，TTL 60s（足够单次 Tick 完成）。
//
// 同一 logical_account_id 的 scheduler / convergence_job / migration_job 共享同一把锁，
// 避免三者并发修改同一 logical 下的 instance 状态。
type RotationLockManager struct {
	lockRepo DistributedLockRepository
	router   *sharding.Router
	ttl      time.Duration
}

// NewRotationLockManager 构造。ttl 推荐 60s（Tick 一次远小于此）。
func NewRotationLockManager(lockRepo DistributedLockRepository, router *sharding.Router, ttl time.Duration) *RotationLockManager {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &RotationLockManager{
		lockRepo: lockRepo,
		router:   router,
		ttl:      ttl,
	}
}

// AcquireForLogical 尝试获取 logical_account_id 的轮换锁。
// 返回 (release func, err)。err != nil 表示锁被其他人持有（contention，不是 db error）
// 或真实 db 错误（也作错误返回；调用方应区分）。
func (m *RotationLockManager) AcquireForLogical(
	ctx context.Context, logicalAccountID int64, owner string,
) (func(), error) {
	lockKey := fmt.Sprintf("rotation:la:%d", logicalAccountID)

	// 路由：用 logical_account_id 哈希到一个 distributed_lock 表分片
	// （所有 lock 都路由到同一分片即可，但为分散热点用 hash）
	dbIdx, gtblIdx := m.router.RouteByID(logicalAccountID)

	acquired, err := m.lockRepo.TryAcquire(ctx, lockKey, owner, m.ttl, dbIdx, gtblIdx)
	if err != nil {
		return nil, fmt.Errorf("rotation lock acquire: %w", err)
	}
	if !acquired {
		return nil, errors.New("rotation lock contention")
	}

	release := func() {
		// 用 background ctx 避免 caller ctx 已 cancel 导致释放失败
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.lockRepo.Release(releaseCtx, lockKey, owner, dbIdx, gtblIdx)
	}
	return release, nil
}

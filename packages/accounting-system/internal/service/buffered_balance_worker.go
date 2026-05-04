package service

// BufferedBalanceWorker 缓冲余额刷新工作器
//
// 工作模式（任务驱动）：
//   - 每个缓冲记账账户在 account_balance_buffer 表中有一条"任务记录"，
//     其中 flush_scheduled_at 标记了下次执行时间。
//   - worker 每隔 BufferFlushInterval（30秒）扫描一次，取出已到期的任务（flush_scheduled_at <= now），
//     每次最多处理 BufferBatchSize（100）个账户，防止大量账户同时到期时产生突发负载。
//   - 刷新完成后：
//       · 缓冲记账账户（buffer_account_config 中配置）→ 重调度：flush_scheduled_at += interval，保留任务记录
//       · 平台/中间账户（仅凭账户类型走缓冲路径）         → 删除行，下次有流水时重新创建
//
// 触发条件（满足任意一条即刷新）：
//   1. flush_scheduled_at <= now（或 IS NULL）  —— 定时任务到期
//   2. pending_count >= BufferFlushThreshold    —— 高频写入，立即刷新（不等待计划时间）
//
// 初始调度（在 tccConfirm 中首次写入时设置）：
//   flush_scheduled_at = now + flush_interval_level + jitter(accountNo)
//   jitter 基于账户号 FNV-32 哈希，均匀分布在 [0, interval/4]，错开不同账户的触发时间。
//
// 重调度规则：
//   next_flush_scheduled_at = old_flush_scheduled_at + interval
//   （基于计划时间而非实际执行时间，防止因执行延迟导致任务漂移）
//
// 幂等性：flush 时用 SELECT FOR UPDATE 保证并发安全；余额更新与任务重调度在同一事务中。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/repository"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// FlushIntervalProvider 提供单个账户的缓冲刷新间隔，用于重调度时计算 next_flush_scheduled_at。
// 由 AccountingService 实现，worker 通过接口读取内存配置，无需额外 DB 查询。
type FlushIntervalProvider interface {
	FlushIntervalForAccount(accountNo string) time.Duration
}

// BufferedBalanceWorker 缓冲余额刷新工作器
type BufferedBalanceWorker struct {
	dbManager        *database.Manager
	router           *sharding.Router
	bufferRepo       repository.BalanceBufferRepository
	intervalProvider FlushIntervalProvider
	logger           *zap.Logger
	stopCh           chan struct{}
	wg               sync.WaitGroup
}

// NewBufferedBalanceWorker 创建工作器
func NewBufferedBalanceWorker(
	dbManager *database.Manager,
	router *sharding.Router,
	bufferRepo repository.BalanceBufferRepository,
	intervalProvider FlushIntervalProvider,
	logger *zap.Logger,
) *BufferedBalanceWorker {
	return &BufferedBalanceWorker{
		dbManager:        dbManager,
		router:           router,
		bufferRepo:       bufferRepo,
		intervalProvider: intervalProvider,
		logger:           logger,
		stopCh:           make(chan struct{}),
	}
}

// Start 启动后台刷新协程（非阻塞）
func (w *BufferedBalanceWorker) Start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx)
	}()
}

// Stop 停止工作器
func (w *BufferedBalanceWorker) Stop() {
	close(w.stopCh)
}

// Wait 阻塞等待 run goroutine 退出。
// 调用顺序：cancel ctx (or Stop) → Wait()。fx OnStop 用 select + deadline 兜底。
func (w *BufferedBalanceWorker) Wait() {
	w.wg.Wait()
}

func (w *BufferedBalanceWorker) run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(model.BufferFlushInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			w.flushAll(ctx)
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// FlushNow 立即触发一次全分片 flush（不等下一个 tick）。
// 用于 admin 接口 / 测试场景，生产路径不调。
func (w *BufferedBalanceWorker) FlushNow(ctx context.Context) { w.flushAll(ctx) }

// flushAll 并发扫描所有分片，处理已到期的缓冲任务。
// 各分片独立并行执行，总耗时 = max(分片耗时)。
func (w *BufferedBalanceWorker) flushAll(ctx context.Context) {
	now := time.Now()
	shards := w.router.GetAllShards()
	var wg sync.WaitGroup
	for _, shard := range shards {
		wg.Add(1)
		go func(dbIndex, tableIndex int) {
			defer wg.Done()
			w.flushShard(ctx, dbIndex, tableIndex, now)
		}(shard.DBIndex, shard.TableIndex)
	}
	wg.Wait()
}

// flushShard 处理单个分片中已到期（flush_scheduled_at <= now）的缓冲任务。
// 每次最多处理 BufferBatchSize 个账户；优先处理高频（pending_count 高）再按到期时间升序。
func (w *BufferedBalanceWorker) flushShard(ctx context.Context, dbIndex, tableIndex int, now time.Time) {
	rows, err := w.bufferRepo.FindDueForFlush(ctx, dbIndex, tableIndex, model.BufferBatchSize, now)
	if err != nil {
		w.logger.Error("buffered balance flush: query buffer failed",
			zap.Int("tableIndex", tableIndex), zap.Error(err))
		return
	}

	db, err := w.dbManager.GetDB(dbIndex)
	if err != nil {
		w.logger.Error("buffered balance flush: get db failed",
			zap.Int("dbIndex", dbIndex), zap.Error(err))
		return
	}
	accountTable := w.router.GetTableName("account", tableIndex)

	for _, row := range rows {
		if err := w.flushRow(ctx, db, row, tableIndex, accountTable, now); err != nil {
			w.logger.Error("buffered balance flush: flush row failed",
				zap.String("accountNo", row.AccountNo),
				zap.Int("tableIndex", tableIndex),
				zap.Error(err))
		}
	}
}

// flushRow 在事务中将单个 buffer 条目的增量刷新到 account，然后重调度或删除任务记录。
func (w *BufferedBalanceWorker) flushRow(ctx context.Context, db *gorm.DB, row model.AccountBalanceBuffer, tableIndex int, accountTable string, now time.Time) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. SELECT buffer 行 FOR UPDATE（防止并发 flush 实例重复刷新）
		locked, err := w.bufferRepo.LockRow(ctx, tx, row.AccountNo, tableIndex)
		if err != nil {
			return fmt.Errorf("lock buffer row: %w", err)
		}
		if locked == nil {
			return nil // 已被其他 flush 处理，幂等退出
		}

		// 2. 有待刷新增量时更新 account.balance
		//    - 正增量（充值/入账）：同步更新 available_balance（未经冻结的资金）
		//    - 负增量（扣款/出账）：available_balance 已在 TCC Try 阶段通过冻结扣减，此处仅更新 balance
		if locked.PendingDelta != 0 || locked.PendingCount > 0 {
			updates := map[string]interface{}{
				"balance": gorm.Expr("balance + ?", locked.PendingDelta),
				"version": gorm.Expr("version + 1"),
			}
			if locked.PendingDelta > 0 {
				updates["available_balance"] = gorm.Expr("available_balance + ?", locked.PendingDelta)
			}
			result := tx.Table(accountTable).
				Where("account_no = ?", row.AccountNo).
				Updates(updates)
			if result.Error != nil {
				return fmt.Errorf("update account balance: %w", result.Error)
			}
			if result.RowsAffected == 0 {
				return fmt.Errorf("account not found: %s", row.AccountNo)
			}
			w.logger.Info("buffered balance flushed",
				zap.String("accountNo", row.AccountNo),
				zap.Int64("delta", locked.PendingDelta),
				zap.Int("count", locked.PendingCount))
		}

		// 3. 重调度 vs 删除
		//    · buffer_account_config 中配置的账户（interval > 默认30秒）：重调度到 scheduled_at + interval
		//      保留行记录，形成循环任务链；next 基于旧 scheduled_at 防止漂移
		//    · 平台/中间账户（未配置，使用默认30秒）：删除行，下次有流水时重新创建
		interval := w.intervalProvider.FlushIntervalForAccount(row.AccountNo)
		isConfiguredBufferAcct := interval > time.Duration(model.BufferFlushInterval)*time.Second

		if isConfiguredBufferAcct {
			// 计算下次计划时间：基于旧 scheduled_at（防止执行延迟导致任务漂移）
			var nextScheduledAt time.Time
			if locked.FlushScheduledAt != nil {
				nextScheduledAt = locked.FlushScheduledAt.Add(interval)
			} else {
				nextScheduledAt = now.Add(interval)
			}
			if err := w.bufferRepo.RescheduleRow(ctx, tx, row.AccountNo, tableIndex, nextScheduledAt); err != nil {
				return fmt.Errorf("reschedule buffer row: %w", err)
			}
			w.logger.Debug("buffer task rescheduled",
				zap.String("accountNo", row.AccountNo),
				zap.Time("nextScheduledAt", nextScheduledAt))
		} else {
			// 平台/中间账户：删除行，下次有流水时由 tccConfirm 重新创建
			if err := w.bufferRepo.DeleteRow(ctx, tx, row.AccountNo, tableIndex); err != nil {
				return fmt.Errorf("delete buffer row: %w", err)
			}
		}

		return nil
	})
}

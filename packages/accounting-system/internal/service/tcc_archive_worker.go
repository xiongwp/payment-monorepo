package service

import (
	"context"
	"sync"
	"time"

	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/metrics"
	"github.com/xiongwp/accounting-system/internal/repository"
	"go.uber.org/zap"
)

// TccArchiveWorker 定期清理 tcc_transaction 表中已终态的分支记录。
//
// 背景:
//   TCC 分支记录只在 Try / Confirm / Cancel 过程中有业务意义，进入 CONFIRMED / CANCELLED
//   后只剩审计价值。不清理会让 tcc_transaction_XX 无限增长，拖慢二级索引（uk_branch_id 等）
//   并增加磁盘占用。账务凭证 / 流水有独立的长期归档（accounting_voucher / account_transaction），
//   因此 tcc_transaction 可以按保留期定期淘汰。
//
// 策略:
//   - 每 TccArchiveInterval（默认 6h）跑一次；
//   - 每轮遍历所有分片，每分片最多 DELETE TccArchiveBatchSize（1000）行；
//     若达到上限说明还有积压，下个 tick 继续，不在单 tick 内死磕。
//   - 仅删除 updated_at < now - TccArchiveRetention（默认 7 天）且 status 终态的行。
//   - DELETE 带 LIMIT 分批，每批几毫秒持锁，对复制延迟友好。
type TccArchiveWorker struct {
	router   *sharding.Router
	tccRepo  repository.TccRepository
	logger   *zap.Logger

	interval  time.Duration
	retention time.Duration
	batchSize int

	wg sync.WaitGroup // 跟踪后台 loop goroutine
}

// 默认配置；生产部署通过 config.yaml tcc_archive.* 覆盖。
const (
	TccArchiveInterval  = 6 * time.Hour
	TccArchiveRetention = 7 * 24 * time.Hour
	TccArchiveBatchSize = 1000
)

// TccArchiveConfig 可运行时调整的归档参数，通常由 main.go 从 viper config 构建。
type TccArchiveConfig struct {
	Interval  time.Duration
	Retention time.Duration
	BatchSize int
}

// NewTccArchiveWorker 构造函数（使用默认配置）。
func NewTccArchiveWorker(router *sharding.Router, tccRepo repository.TccRepository, logger *zap.Logger) *TccArchiveWorker {
	return NewTccArchiveWorkerWithConfig(router, tccRepo, logger, TccArchiveConfig{
		Interval:  TccArchiveInterval,
		Retention: TccArchiveRetention,
		BatchSize: TccArchiveBatchSize,
	})
}

// NewTccArchiveWorkerWithConfig 构造函数（显式配置）。零值字段 fallback 到默认值。
func NewTccArchiveWorkerWithConfig(router *sharding.Router, tccRepo repository.TccRepository, logger *zap.Logger, cfg TccArchiveConfig) *TccArchiveWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = TccArchiveInterval
	}
	if cfg.Retention <= 0 {
		cfg.Retention = TccArchiveRetention
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = TccArchiveBatchSize
	}
	return &TccArchiveWorker{
		router:    router,
		tccRepo:   tccRepo,
		logger:    logger,
		interval:  cfg.Interval,
		retention: cfg.Retention,
		batchSize: cfg.BatchSize,
	}
}

// Config 返回当前生效的运行时配置，供 admin-web 查询展示。
func (w *TccArchiveWorker) Config() TccArchiveConfig {
	return TccArchiveConfig{Interval: w.interval, Retention: w.retention, BatchSize: w.batchSize}
}

// ArchiveNow 手动触发一次归档扫描（admin-web 立即归档按钮用）。
// 与定时 tick 共享 runOnce，线程安全由 runOnce 的每分片串行保证。
// ctx 建议是独立 context（而非请求 ctx），避免调用方提前断开导致清理中断。
func (w *TccArchiveWorker) ArchiveNow(ctx context.Context) {
	w.runOnce(ctx)
}

// Start 启动后台循环。ctx 取消时退出。
func (w *TccArchiveWorker) Start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// 启动时延迟一个 interval 再跑，避免部署瞬间打爆 DB
		timer := time.NewTimer(w.interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				w.runOnce(ctx)
				timer.Reset(w.interval)
			}
		}
	}()
	w.logger.Info("tcc archive worker started",
		zap.Duration("interval", w.interval),
		zap.Duration("retention", w.retention),
		zap.Int("batchSize", w.batchSize),
	)
}

// Wait 阻塞等待后台 loop 退出。fx OnStop 用 cancel + Wait 配合 deadline 实现 graceful drain。
func (w *TccArchiveWorker) Wait() {
	w.wg.Wait()
}

// runOnce 扫描全部分片，每分片最多删除 batchSize 行。不同分片串行，避免 DB 并发压力。
// 单分片若删到 batchSize 说明未清完，下个 interval 继续。
func (w *TccArchiveWorker) runOnce(ctx context.Context) {
	olderThan := time.Now().Add(-w.retention)
	totalDeleted := int64(0)
	shards := w.router.GetAllShards()
	for _, shard := range shards {
		select {
		case <-ctx.Done():
			return
		default:
		}
		deleted, err := w.tccRepo.ArchiveTerminalBranches(ctx, shard.DBIndex, shard.TableIndex, olderThan, w.batchSize)
		if err != nil {
			w.logger.Warn("tcc archive: shard delete failed",
				zap.Int("dbIndex", shard.DBIndex),
				zap.Int("tableIndex", shard.TableIndex),
				zap.Error(err))
			metrics.TccArchiveErrorsTotal.Inc()
			continue
		}
		if deleted > 0 {
			totalDeleted += deleted
			metrics.TccArchivedTotal.Add(float64(deleted))
		}
	}
	if totalDeleted > 0 {
		w.logger.Info("tcc archive completed a pass",
			zap.Int64("totalDeleted", totalDeleted),
			zap.Time("cutoff", olderThan),
		)
	}
}

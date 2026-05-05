package service

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/metrics"
	"github.com/xiongwp/order-core/internal/repo"
)

// AccountingClient 抽象 accounting-system 的 HybridDoubleEntryBooking 调用，
// 便于单测注入 mock（内部实现走 grpc-go，见 internal/accounting/client.go）。
type AccountingClient interface {
	DoubleEntryBooking(ctx context.Context, row *domain.AccountingOutbox) error
}

// AccountingOutboxWorker 轮询 pending outbox 并投递给 accounting-system。
type AccountingOutboxWorker struct {
	outboxRepo repo.AccountingOutboxRepository
	client     AccountingClient
	logger     *zap.Logger

	batchSize    int
	pollInterval time.Duration
	maxAttempts  int
	baseBackoff  time.Duration
	maxBackoff   time.Duration

	// wake 由 AccountingOutboxService.Enqueue* 触发，让 worker 立刻 Tick 而不是
	// 等下一个 poll interval。缓冲 1 保证并发 enqueue 不阻塞 service；多次
	// signal 合并成一次 Tick 没有副作用。
	wake chan struct{}
}

// AccountingOutboxWorkerConfig 可调参数。
type AccountingOutboxWorkerConfig struct {
	BatchSize    int
	PollInterval time.Duration
	MaxAttempts  int
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
}

// NewAccountingOutboxWorker 构造。client 为 nil 时 worker 保持空转（feature flag off），
// outbox 行保留 pending 直到客户端到位；用于在 accounting endpoint 未配置时仍让
// webhook 路径继续写 outbox，稍后再补消费。
func NewAccountingOutboxWorker(outboxRepo repo.AccountingOutboxRepository, client AccountingClient, logger *zap.Logger, cfg AccountingOutboxWorkerConfig) *AccountingOutboxWorker {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 10
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 2 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 5 * time.Minute
	}
	return &AccountingOutboxWorker{
		outboxRepo:   outboxRepo,
		client:       client,
		logger:       logger,
		batchSize:    cfg.BatchSize,
		pollInterval: cfg.PollInterval,
		maxAttempts:  cfg.MaxAttempts,
		baseBackoff:  cfg.BaseBackoff,
		maxBackoff:   cfg.MaxBackoff,
		wake:         make(chan struct{}, 1),
	}
}

// Wake 让 enqueue 侧触发一次即时 Tick；非阻塞、去重。
// 典型路径：services.go 同步 succeeded 分支 → Enqueue* → worker.Wake() →
// 立刻投递到 accounting-system，不用等下一个 5s poll。
func (w *AccountingOutboxWorker) Wake() {
	if w == nil || w.wake == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// outboxMaxIdleInterval 自适应轮询的最大空闲间隔。空载时从 pollInterval 指数翻倍，
// 封顶到这个值；有活时立即重置回 pollInterval。
//
// **P2-9 调整**：原值 30s 在跨进程写入场景下让 refund 等关键路径平均多等 15s
// 才被消费（pollInterval=5s → 5,10,20,30,30…，期望响应 ~17s）。
// 改成 10s（5,10,10,10…）让最大延迟收敛到 ≤10s，CPU 开销提升约 1.5×（每分钟 6 次空扫
// → 12 次），可接受。Wake 信号正常工作时 idle interval 几乎不触发。
const outboxMaxIdleInterval = 10 * time.Second

// outboxProcessConcurrency 单次 Tick 内并发处理 batch 的 goroutine 上限。
// 4 是经验值：accounting-system 单 voucher TCC 是单 db 事务，4-way 并发能让
// 不同 voucher 跨分片并行，又不会一口气压垮 connection pool。
// 同 voucher 的多行（极罕见）天然按 voucher 路由到同分片，串行也无伤大雅。
const outboxProcessConcurrency = 4

// Run 阻塞运行直到 ctx.Done()。
//
// 自适应轮询：
//   - 满批（== batchSize）→ 下一轮立即 Tick（不等 ticker），表示积压未消化完
//   - 部分批  → 维持当前间隔
//   - 空批  → 间隔 ×2 直到 outboxMaxIdleInterval；下次有活时立即 reset 到 pollInterval
//   - Wake 信号 → 立即 Tick 并 reset 间隔（enqueue 侧主动通知）
//
// 这样日常无积压时 worker 空转开销极低，突发负载下又能瞬时响应。
func (w *AccountingOutboxWorker) Run(ctx context.Context) {
	w.logger.Info("accounting outbox worker started",
		zap.Duration("poll", w.pollInterval),
		zap.Int("batch", w.batchSize))

	interval := w.pollInterval
	metrics.AcctOutboxTickInterval.Set(interval.Seconds())

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("accounting outbox worker stopped")
			return
		default:
		}

		fetched := w.Tick(ctx)
		switch {
		case fetched >= w.batchSize:
			// 满批：积压未消化，立刻继续，不退避
			interval = w.pollInterval
		case fetched > 0:
			// 部分批：保持基线
			interval = w.pollInterval
		default:
			// 空批：×2 退避，封顶 max
			interval *= 2
			if interval > outboxMaxIdleInterval {
				interval = outboxMaxIdleInterval
			}
		}
		metrics.AcctOutboxTickInterval.Set(interval.Seconds())

		// 满批时不睡，立刻下一轮（仍可被 ctx 抢占 / wake 唤醒）
		if fetched >= w.batchSize {
			continue
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			w.logger.Info("accounting outbox worker stopped")
			return
		case <-timer.C:
		case <-w.wake:
			timer.Stop()
			interval = w.pollInterval
			metrics.AcctOutboxTickInterval.Set(interval.Seconds())
		}
	}
}

// outboxClaimLease 单次 ClaimBatch 的租约时长。
// process() 内一次 accounting RPC 期望 < 1s，4-way 并发处理 batchSize=100 行
// 最坏 ~30s 完成；留 5min 余量足够，租约过期后下一轮 worker（甚至本 worker）
// 可重新 claim 该行重试。
const outboxClaimLease = 5 * time.Minute

// outboxPerTableClaim 单张分片表单次 claim 上限。
// 总量上限 = perTable × 100 张表；perTable=batchSize/100 让总量贴近 batchSize。
// 极端情况下 100 张表都打满 → 100 × perTable 行；这也比 ListPending 单点 limit 公平
// （旧实现 LIMIT 100 会偏向遍历到第一张满载的表，其余表饿死）。
func (w *AccountingOutboxWorker) perTableClaimLimit() int {
	per := w.batchSize / 100
	if per < 1 {
		per = 1
	}
	return per
}

// Tick 执行一轮：claim 一批 due 行 → 并发调 client → 回写状态。返回本轮 claim 的行数（用于自适应退避）。
//
// 多副本安全：通过 ClaimBatch 的原子 UPDATE，同一行只能被一个 worker claim；
// 即使两个 pod 同时进入 Tick 也不会重复处理。
func (w *AccountingOutboxWorker) Tick(ctx context.Context) int {
	// 先采集 pending stats 喂 lag metric（COUNT + MIN(created) 跨 100 张表）。
	// 失败不阻断正常 claim 流程；监控可以接受偶尔丢点。
	if cnt, oldest, err := w.outboxRepo.StatsPending(ctx); err == nil {
		metrics.AcctOutboxPendingGauge.Set(float64(cnt))
		if oldest.IsZero() {
			metrics.AcctOutboxOldestPendingAgeSeconds.Set(0)
		} else {
			age := time.Since(oldest).Seconds()
			if age < 0 {
				age = 0
			}
			metrics.AcctOutboxOldestPendingAgeSeconds.Set(age)
		}
	}

	claimToken, claimed, err := w.outboxRepo.ClaimBatch(ctx, time.Now(), w.perTableClaimLimit(), outboxClaimLease)
	if err != nil {
		w.logger.Error("claim batch accounting outbox", zap.Error(err))
		return 0
	}
	metrics.AcctOutboxBatchSize.Observe(float64(claimed))
	if claimed == 0 {
		return 0
	}
	rows, err := w.outboxRepo.ListByClaimToken(ctx, claimToken)
	if err != nil {
		w.logger.Error("list by claim_token", zap.String("token", claimToken), zap.Error(err))
		// 不 return — 已 claim 的行租约会自然过期重新被抢，不会卡死
		return claimed
	}
	if w.client == nil {
		w.logger.Debug("accounting client not configured; leaving rows claimed (lease will expire)",
			zap.Int("count", len(rows)))
		// client 未就绪：不 process。租约过期后会重回 pending；
		// 启动期短暂未配置 endpoint 时的常见路径。
		return len(rows)
	}
	// 并发处理 batch：semaphore 限到 outboxProcessConcurrency。
	// 与 webhook dispatcher 的并发模式一致；在不打爆 accounting connection pool 的
	// 前提下让不同 voucher 的 booking 并行执行。
	sem := make(chan struct{}, outboxProcessConcurrency)
	var wg sync.WaitGroup
	for _, row := range rows {
		row := row
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			w.process(ctx, row)
		}()
	}
	wg.Wait()
	return len(rows)
}

func (w *AccountingOutboxWorker) process(ctx context.Context, row *domain.AccountingOutbox) {
	err := w.client.DoubleEntryBooking(ctx, row)
	if err == nil {
		metrics.AcctOutboxProcessTotal.WithLabelValues("ok").Inc()
		if markErr := w.outboxRepo.MarkSent(ctx, row); markErr != nil {
			w.logger.Error("mark sent failed", zap.String("id", row.ID), zap.Error(markErr))
		}
		return
	}
	attempts := row.Attempts + 1
	if attempts >= w.maxAttempts {
		metrics.AcctOutboxProcessTotal.WithLabelValues("exhausted").Inc()
		if markErr := w.outboxRepo.MarkFailed(ctx, row, err.Error()); markErr != nil {
			w.logger.Error("mark failed failed", zap.String("id", row.ID), zap.Error(markErr))
		}
		w.logger.Warn("accounting outbox exhausted retries",
			zap.String("id", row.ID),
			zap.Int("attempts", attempts),
			zap.Error(err))
		return
	}
	metrics.AcctOutboxProcessTotal.WithLabelValues("retry").Inc()
	// 指数退避：base × 2^attempts，封顶 maxBackoff。
	// 老代码用 `w.baseBackoff << attempts` 在 attempts 大且 baseBackoff 为非 power-of-2
	// duration 时含义不对（int64 左移不等价于 ×2^n on time.Duration units）。
	// 用乘法显式：base × 2^attempts，attempts 已被 maxAttempts(=10) 限制，不会溢出。
	backoff := w.baseBackoff
	for i := 0; i < attempts; i++ {
		backoff *= 2
		if backoff > w.maxBackoff {
			backoff = w.maxBackoff
			break
		}
	}
	next := time.Now().Add(backoff)
	if markErr := w.outboxRepo.MarkRetry(ctx, row, next, err.Error()); markErr != nil {
		w.logger.Error("mark retry failed", zap.String("id", row.ID), zap.Error(markErr))
	}
}

// AccountingOutboxArchiver 周期性清理已投递的 outbox 行 + 上报 dead-letter 指标。
// 单独开 worker（轮询周期 1 小时）和主 worker 解耦：
//   - 主 worker 高频轮询 pending 行（5s）
//   - 归档低频（1h）避免删除和主 worker 扫描抢锁
// 保留天数 retain + TTL 超过后删 status=sent 的行，status=failed 永远不删。
type AccountingOutboxArchiver struct {
	outboxRepo repo.AccountingOutboxRepository
	logger     *zap.Logger

	retain       time.Duration // sent 行保留时长，超出删除
	pollInterval time.Duration // 扫描周期
	batch        int           // 每次扫描每张表最多删除数
}

// AccountingOutboxArchiverConfig 归档 worker 可调参数。
type AccountingOutboxArchiverConfig struct {
	Retain       time.Duration // default 7 * 24h
	PollInterval time.Duration // default 1h
	Batch        int           // default 1000
}

// NewAccountingOutboxArchiver 构造。
func NewAccountingOutboxArchiver(
	outboxRepo repo.AccountingOutboxRepository,
	logger *zap.Logger,
	cfg AccountingOutboxArchiverConfig,
) *AccountingOutboxArchiver {
	if logger == nil {
		logger = zap.NewNop()
	}
	if cfg.Retain <= 0 {
		cfg.Retain = 7 * 24 * time.Hour
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Hour
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 1000
	}
	return &AccountingOutboxArchiver{
		outboxRepo:   outboxRepo,
		logger:       logger,
		retain:       cfg.Retain,
		pollInterval: cfg.PollInterval,
		batch:        cfg.Batch,
	}
}

// Tick 手动触发一轮（测试 / admin 调试）。返回本轮删除的 sent 行数 + 当前 dead-letter 总数。
func (w *AccountingOutboxArchiver) Tick(ctx context.Context) (deleted, deadLetters int64) {
	before := time.Now().UTC().Add(-w.retain)
	n, err := w.outboxRepo.PurgeSentBefore(ctx, before, w.batch)
	if err != nil {
		w.logger.Error("accounting outbox purge failed", zap.Error(err))
	} else if n > 0 {
		w.logger.Info("accounting outbox purged sent",
			zap.Int64("rows", n),
			zap.Time("sent_before", before))
	}
	dl, err := w.outboxRepo.CountDeadLetters(ctx)
	if err != nil {
		w.logger.Error("accounting outbox dead-letter count failed", zap.Error(err))
	} else if dl > 0 {
		w.logger.Warn("accounting outbox dead letters present",
			zap.Int64("count", dl))
	}
	return n, dl
}

// Run 后台循环。Close 或 ctx Done 时退出。
func (w *AccountingOutboxArchiver) Run(ctx context.Context) {
	w.logger.Info("accounting outbox archiver started",
		zap.Duration("retain", w.retain),
		zap.Duration("poll", w.pollInterval))
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("accounting outbox archiver stopping")
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

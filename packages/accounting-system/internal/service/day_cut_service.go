package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	commonutil "github.com/accounting-system/internal/common"
	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/infrastructure/database"
	"github.com/accounting-system/internal/infrastructure/sharding"
	"github.com/accounting-system/internal/metrics"
	"github.com/accounting-system/internal/repository"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// dayCutDrainTimeoutTotal 记录 drain 等待超时事件。每发生一次 inc 一次
// 由 currency 维度区分。alerting：rate > 0 任意时刻都需要立刻拉警，
// 因为 day cut 已被 abort，运维必须重跑（先清干净 stuck TCC）。
var dayCutDrainTimeoutTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "accounting_day_cut_drain_timeout_total",
	Help: "Day cut aborted because in-flight TCC didn't settle within drain timeout. ALERT: snapshot would be inconsistent if proceeded.",
}, []string{"currency"})

// DayCutHistoryEntry holds the aggregated day-cut status for a single
// (cut_date, run_id, currency) across all shards. currency 是这次 run 的币种
// 过滤；空值表示历史数据（未带币种 filter 的老 run）。
type DayCutHistoryEntry struct {
	CutDate     string `json:"cut_date"`
	RunID       int    `json:"run_id"`
	Currency    string `json:"currency"`
	TotalShards int    `json:"total_shards"`
	Pending     int    `json:"pending"`
	Processing  int    `json:"processing"`
	Completed   int    `json:"completed"`
	Failed      int    `json:"failed"`
}

// DayCutService 日切服务接口
type DayCutService interface {
	// TriggerDayCut 触发日切（每次调用 run_id 自增，支持重跑）。
	// currency 必填：每次只对指定币种的账户做快照。不同币种汇总没有意义，
	// 必须每个币种独立跑一次。
	TriggerDayCut(ctx context.Context, cutDate, currency string) error

	// ProcessShardDayCut 处理单个分片的日切。currency 必填（仅快照该币种账户）。
	// 恢复场景从 day_cut_control.currency 读取原值回传，保证和初次触发时一致。
	ProcessShardDayCut(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, currency string) error

	// CheckDayCutStatus 检查日切状态
	CheckDayCutStatus(ctx context.Context, cutDate string) (map[string]interface{}, error)

	// ListDayCutHistory returns historical day-cut entries grouped by (cut_date, run_id), sorted DESC.
	ListDayCutHistory(ctx context.Context) ([]*DayCutHistoryEntry, error)

	// GetLatestCompletedRunID returns the highest run_id for cutDate where ALL shards are COMPLETED.
	// Returns found=false if no fully-completed run exists.
	GetLatestCompletedRunID(ctx context.Context, cutDate string) (runID int, found bool, err error)

	// WatchdogRecover scans today's day-cut shards and re-launches any shard that has been
	// stuck in PENDING or PROCESSING status beyond stuckThreshold.
	// Safe to call concurrently; each shard runs in an independent goroutine.
	WatchdogRecover(ctx context.Context, stuckThreshold time.Duration) error

	// ResumeStuckShards 跨所有分片扫描指定 (cutDate, runID) 中处于 PROCESSING 且
	// updated_at 早于 stuckThreshold 的"无 worker 跑的卡死"行，重新派发 ProcessShardDayCut。
	//
	// 与 WatchdogRecover 区别：
	//   - WatchdogRecover 只看每个分片的 latest run_id（典型场景：今日定时任务）
	//   - ResumeStuckShards 接收明确的 (cutDate, runID)，可恢复任意一次重跑（v5/v6 等）
	//
	// 用例：服务启动时一次性扫描；admin-web "恢复卡死分片"按钮。
	// stuckThreshold = 0 表示不过滤 updated_at（强制恢复所有 PROCESSING）。
	// 返回成功重新派发的分片数（不等待执行完成，goroutine 异步跑）。
	ResumeStuckShards(ctx context.Context, cutDate string, runID int, stuckThreshold time.Duration) (int, error)
}

type dayCutService struct {
	router              *sharding.Router
	dbManager           *database.Manager
	transactionRepo     repository.TransactionRepository
	dayCutControlRepo   repository.DayCutControlRepository
	balanceSnapshotRepo repository.BalanceSnapshotRepository
	accountRepo         repository.AccountRepository
	bufferRepo          repository.BalanceBufferRepository
	outboxRepo          repository.SettlementOutboxRepository
	orderRepo           repository.TransactionOrderRepository // 审计/恢复用；nil-safe
	tccCoordRepo        repository.TccCoordinatorRepository   // drain wait 检测 in-flight CONFIRMING coords
	logger              *zap.Logger
}

// NewDayCutService 创建日切服务
func NewDayCutService(
	router *sharding.Router,
	dbManager *database.Manager,
	transactionRepo repository.TransactionRepository,
	dayCutControlRepo repository.DayCutControlRepository,
	balanceSnapshotRepo repository.BalanceSnapshotRepository,
	accountRepo repository.AccountRepository,
	bufferRepo repository.BalanceBufferRepository,
	outboxRepo repository.SettlementOutboxRepository,
	orderRepo repository.TransactionOrderRepository,
	tccCoordRepo repository.TccCoordinatorRepository,
	logger *zap.Logger,
) DayCutService {
	return &dayCutService{
		router:              router,
		dbManager:           dbManager,
		transactionRepo:     transactionRepo,
		dayCutControlRepo:   dayCutControlRepo,
		balanceSnapshotRepo: balanceSnapshotRepo,
		accountRepo:         accountRepo,
		bufferRepo:          bufferRepo,
		outboxRepo:          outboxRepo,
		orderRepo:           orderRepo,
		tccCoordRepo:        tccCoordRepo,
		logger:              logger,
	}
}

// drainWaitTimeout day-cut 触发后等待 in-flight CONFIRMING coord 落定的最大时长。
// 60s 覆盖 TCC 三阶段 + 一次 retry 的 wall-clock 上限；超时仍未 0 则继续扫描并打 WARN —
// 残留的 CONFIRMING coord 在下一次 cut 会被 recovery 拾起，不会丢账，但当次 cut
// 可能漏到该 coord 对应的 voucher 部分 entry（若 in-flight 跨 cut_date 边界）。
const drainWaitTimeout = 60 * time.Second

// drainWaitInterval 每次 poll 间隔。
const drainWaitInterval = 1 * time.Second

// waitCoordsDrained 等待所有 cut_date <= maxCutDate 的 in-flight (TRYING/CONFIRMING)
// coord 落定。0 → 安全扫描；超时 → 返回 false，调用方决定是否继续。
func (s *dayCutService) waitCoordsDrained(ctx context.Context, maxCutDate string) bool {
	if s.tccCoordRepo == nil {
		return true
	}
	deadline := time.Now().Add(drainWaitTimeout)
	for {
		n, err := s.tccCoordRepo.CountActiveByCutDate(ctx, maxCutDate)
		if err != nil {
			s.logger.Warn("drain wait: count active coords failed", zap.Error(err))
			return false
		}
		if n == 0 {
			return true
		}
		if time.Now().After(deadline) {
			s.logger.Warn("drain wait: timeout, proceeding with possibly-stale set",
				zap.String("max_cut_date", maxCutDate),
				zap.Int64("active_coords", n),
				zap.Duration("waited", drainWaitTimeout))
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(drainWaitInterval):
		}
	}
}

// TriggerDayCut 触发日切。每次调用 run_id 自增，支持多次重跑。
// currency 必填：每次只对该币种账户做快照，其他币种完全不触碰。
// 不同币种独立跑，各自一个 (cut_date, currency, run_id)。
func (s *dayCutService) TriggerDayCut(ctx context.Context, cutDate, currency string) error {
	if currency == "" {
		return fmt.Errorf("day_cut: currency is required (per-currency execution only)")
	}
	s.logger.Info("day cut started",
		zap.String("cutDate", cutDate),
		zap.String("currency", currency))

	shards := s.router.GetAllShards()

	// 确定本次运行的 run_id：并发读取所有分片的 max(run_id)，取全局最大值 + 1
	type maxIDResult struct {
		maxID int
		err   error
	}
	maxIDResults := make([]maxIDResult, len(shards))
	{
		var wg sync.WaitGroup
		for i, shard := range shards {
			i, shard := i, shard
			wg.Add(1)
			go func() {
				defer wg.Done()
				id, err := s.dayCutControlRepo.GetMaxRunID(ctx, shard.DBIndex, shard.TableIndex, cutDate)
				maxIDResults[i] = maxIDResult{maxID: id, err: err}
			}()
		}
		wg.Wait()
	}
	newRunID := 1
	for i, res := range maxIDResults {
		if res.err != nil {
			shard := shards[i]
			return fmt.Errorf("get max run_id for shard(%d,%d) failed: %w", shard.DBIndex, shard.TableIndex, res.err)
		}
		if res.maxID+1 > newRunID {
			newRunID = res.maxID + 1
		}
	}

	s.logger.Info("day cut run_id determined",
		zap.String("cutDate", cutDate),
		zap.Int("runID", newRunID),
	)

	// 并发初始化所有分片的日切控制记录（100 分片串行 Upsert → 并发 Upsert）
	{
		type upsertErr struct{ err error; dbIndex, tableIndex int }
		upsertErrs := make([]upsertErr, len(shards))
		var wg sync.WaitGroup
		for i, shard := range shards {
			i, shard := i, shard
			wg.Add(1)
			go func() {
				defer wg.Done()
				txTableName := fmt.Sprintf("account_transaction_%02d", shard.TableIndex)
				// CutAtLower/CutAtUpper 必须显式给值——MySQL strict_mode 下
				// time.Time{} 零值（0001-01-01）会被当成 '0000-00-00' 拒收。
				// CutMinID/CutMaxID/CutMaxOrderID 都是 BIGINT NOT NULL DEFAULT 0；
				// 0 表示"未锁定"，ProcessShardDayCut 第一次跑时通过 lockCutWatermark
				// CAS 推进到真实值。BIGINT DEFAULT 0 不存在 time.Time{} 那样的
				// strict-mode 0000-00-00 拒收问题。
				record := &model.DayCutControl{
					DatabaseIndex: shard.DBIndex,
					TableIndex:    shard.TableIndex,
					TableName:     txTableName,
					CutDate:       cutDate,
					RunID:         newRunID,
					Currency:      currency,
					Status:        model.DayCutStatusPending,
				}
				if err := s.dayCutControlRepo.Upsert(ctx, shard.DBIndex, shard.TableIndex, record); err != nil {
					upsertErrs[i] = upsertErr{err: err, dbIndex: shard.DBIndex, tableIndex: shard.TableIndex}
				}
			}()
		}
		wg.Wait()
		for _, ue := range upsertErrs {
			if ue.err != nil {
				return fmt.Errorf("init day cut control for shard(%d,%d) failed: %w", ue.dbIndex, ue.tableIndex, ue.err)
			}
		}
	}

	// drain 等待：所有 cut_date <= 本次 cutDate 的 in-flight coord（TRYING/CONFIRMING）
	// 必须落定（CONFIRMED 或 CANCELLED）才开始扫描。这样 WHERE cut_date=cutDate 看到
	// 的 entry 集合就是 final state，试算平衡天然成立。
	//
	// **资金安全 fail-fast**：超时 fail，不继续打 snapshot。如果 in-flight TCC
	// 跨过 boundary，snapshot 拍到的 account.balance 会是 stale（TCC mid-Confirm
	// 还没改 balance），与 account_transaction 期末值对不平 → trial balance 报不平。
	// 之前 silent log warn 后继续会让运维深夜盯第二天告警，资损追溯麻烦。
	// 现在直接报错 + Prometheus counter，让运维立即介入（重跑 day cut / 触发
	// /admin/tcc/retry-confirm 把 stuck CONFIRMING 推完）。
	if drained := s.waitCoordsDrained(ctx, cutDate); !drained {
		dayCutDrainTimeoutTotal.WithLabelValues(currency).Inc()
		s.logger.Error("CRITICAL: day cut aborted — drain timeout, in-flight TCC may corrupt snapshot consistency",
			zap.String("cutDate", cutDate),
			zap.String("currency", currency),
			zap.String("remediation", "trigger /admin/tcc/retry-confirm or wait for recovery worker, then re-run day cut"))
		return fmt.Errorf("day cut aborted (cutDate=%s currency=%s): drain timeout — in-flight TCC must settle before snapshot", cutDate, currency)
	}

	// 并发处理所有分片。
	// 使用 context.WithoutCancel(ctx)：脱离 HTTP 请求取消信号（避免 caller 断开导致分片停在 PENDING），
	// 但保留 ctx 上承载的 trace / span / deadline 元信息，便于观测。
	//
	// **P1-19 强制清 shadow flag**：context.WithoutCancel 会保留 ctx.Value 包括
	// shadow=true。日切如果由 shadow trigger（admin 误操作 / 压测端点）触发，会让
	// 整个全分片日切落到 _shadow 表，主流量当天 trial balance / cut_date snapshot
	// 全部缺失。强制 WithoutShadow 把 shadow flag 显式抹掉——日切永远只对主流量生效。
	// shadow 流量自己的日切应通过独立的 admin endpoint 显式 trigger（见 admin handler）。
	//
	// 超时 dayCutShardTimeout 作为硬兜底；正常每个分片 batch+checkpoint 几分钟即可完成，
	// 4h 是旧的宽松值。收紧到 30min → 若真卡住可触发 watchdog 及时重新派发。
	bgCtx, cancel := context.WithTimeout(shadow.WithShadow(context.WithoutCancel(ctx), false), dayCutShardTimeout)

	// 并发进度计数（atomic 安全）。每 30s 打印一次仍在跑的分片数 + 已完成数。
	// 在 100 分片并发时，单分片完成日志容易被淹没；这里给运维提供"全局进度"视图。
	var (
		completed int64
		failed    int64
	)
	totalShards := int64(len(shards))
	progressDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-ticker.C:
				done := atomic.LoadInt64(&completed)
				fail := atomic.LoadInt64(&failed)
				running := totalShards - done - fail
				s.logger.Info("day cut progress",
					zap.String("cutDate", cutDate),
					zap.Int("runID", newRunID),
					zap.Int64("completed", done),
					zap.Int64("failed", fail),
					zap.Int64("running", running),
					zap.Int64("total", totalShards),
					zap.Float64("progressPct", float64(done+fail)/float64(totalShards)*100),
				)
			}
		}
	}()

	// P1-14 并发度限流：不限并发的话 100 goroutine 同时握 100 张分片表 + 100
	// 个 DB 事务 → 把 DB 连接池吃光，反而比串行还慢（context switch + lock 等待）。
	// 上限 10：每分库 1 路并发（10 库 × 1）≈ 单分片 RT × 10 总耗时，恰好平衡。
	const dayCutFanoutConcurrency = 10
	sem := make(chan struct{}, dayCutFanoutConcurrency)
	var wg sync.WaitGroup
	for _, shard := range shards {
		wg.Add(1)
		sem <- struct{}{}
		go func(dbIdx, tableIdx int) {
			defer wg.Done()
			defer func() { <-sem }()
			shardStart := time.Now()
			if err := s.ProcessShardDayCut(bgCtx, dbIdx, tableIdx, cutDate, newRunID, currency); err != nil {
				atomic.AddInt64(&failed, 1)
				s.logger.Error("shard day cut failed",
					zap.Int("dbIndex", dbIdx),
					zap.Int("tableIndex", tableIdx),
					zap.String("cutDate", cutDate),
					zap.Int("runID", newRunID),
					zap.Duration("elapsed", time.Since(shardStart)),
					zap.Error(err),
				)
				return
			}
			atomic.AddInt64(&completed, 1)
		}(shard.DBIndex, shard.TableIndex)
	}

	// 所有分片完成后释放 cancel + 终止进度日志 goroutine
	go func() {
		wg.Wait()
		close(progressDone)
		cancel()
		s.logger.Info("day cut completed",
			zap.String("cutDate", cutDate),
			zap.Int("runID", newRunID),
			zap.Int64("completed", atomic.LoadInt64(&completed)),
			zap.Int64("failed", atomic.LoadInt64(&failed)),
		)
	}()

	s.logger.Info("day cut triggered, processing in background",
		zap.String("cutDate", cutDate),
		zap.Int("runID", newRunID),
	)
	return nil
}

// dayCutChunkSize 单次 SELECT 返回的最大行数。
//
// 设计取舍：
//   - 太小 → 多次 round-trip
//   - 太大 → 单批内存峰值高，崩溃后重复工作多
// 5000 行典型负载下：扫描耗时数十 ms、内存 ~5MB（1KB/行 × 5K）。
// 通过 LIMIT + WHERE id > last_processed_id 链式分块，覆盖窗口内所有数据。
const dayCutChunkSize int = 5000

// truncateToDay 把 cutDate (YYYY-MM-DD) 解析为本地时区 00:00:00.000。
// 保留作为 cut_date 参数有效性校验的工具；id-watermark 模式下不再用作扫描边界。
func truncateToDay(cutDate string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", cutDate, time.Local)
}

// ProcessShardDayCut 处理单个分片的日切（cut_date tag 版）。
//
// 流程：
//  1. 幂等检查：已完成跳过；置 PROCESSING
//  2. 读 last_processed_id 作为续跑游标
//  3. 循环 chunk：WHERE cut_date=? AND status=SUCCESS AND id > cursor LIMIT N
//      - 每 chunk 内事务原子提交：UpsertBatchIncremental + SetLastProcessedID
//  4. finalize buffered 账户（同样按 cut_date）
//  5. UpdateStatusCompleted
//
// 试算平衡正确性：booking 入口处一次性确定 cut_date 并 propagate 到所有 entry +
// tcc_coordinator，同一 voucher 全部 entry 共享同一 cut_date → WHERE cut_date=X
// 整张凭证全入或全不入 → SUM(DR)=SUM(CR)。drain 等待由 TriggerDayCut 在调度层
// 完成（CountActiveByCutDate=0 才进入分片处理），扫描看到的就是 final state。
//
// 崩溃恢复：next call 看到 PROCESSING + 已持久化 last_processed_id → 直接从游标后继续。
func (s *dayCutService) ProcessShardDayCut(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int, currency string) error {
	startTime := time.Now()
	logBase := []zap.Field{
		zap.Int("dbIndex", dbIndex),
		zap.Int("tableIndex", tableIndex),
		zap.String("cutDate", cutDate),
		zap.Int("runID", runID),
		zap.String("currency", currency),
	}
	s.logger.Info("processing shard day cut", logBase...)

	// 1. 幂等守卫
	existing, err := s.dayCutControlRepo.GetByShard(ctx, dbIndex, tableIndex, cutDate, runID)
	if err != nil {
		return fmt.Errorf("check day cut status: %w", err)
	}
	if existing != nil && existing.Status == model.DayCutStatusCompleted {
		s.logger.Info("shard day cut already completed, skipping", logBase...)
		return nil
	}
	if err := s.dayCutControlRepo.UpdateStatus(ctx, dbIndex, tableIndex, cutDate, runID, model.DayCutStatusProcessing, "", nil); err != nil {
		return err
	}

	// 2. 续跑游标
	var cursor uint64
	if existing != nil {
		cursor = existing.LastProcessedID
	}
	s.logger.Info("starting shard day cut by cut_date",
		append(logBase, zap.Uint64("cursorStart", cursor))...)

	// 5. 分段循环：每次拉 chunkSize 行，处理完推进游标，直到一个 chunk 返回 < chunkSize 行
	chunkIdx := 0
	totalTxProcessed := 0
	var lastTransactionID string
	for {
		select {
		case <-ctx.Done():
			s.dayCutControlRepo.UpdateStatus(ctx, dbIndex, tableIndex, cutDate, runID, model.DayCutStatusFailed, "ctx cancelled mid-chunk", &startTime) //nolint:errcheck
			return ctx.Err()
		default:
		}
		chunkIdx++
		chunkStart := time.Now()
		txs, err := s.transactionRepo.ListByCutDateChunk(ctx, dbIndex, tableIndex, cutDate, currency, cursor, dayCutChunkSize)
		if err != nil {
			s.dayCutControlRepo.UpdateStatus(ctx, dbIndex, tableIndex, cutDate, runID, model.DayCutStatusFailed, err.Error(), &startTime) //nolint:errcheck
			return fmt.Errorf("list by cut_date chunk (cut_date=%s id>%d): %w", cutDate, cursor, err)
		}
		if len(txs) == 0 {
			break
		}

		// 计算本 chunk 的局部 stats（per-chunk 内存上限 ~ chunkSize × 行字节）
		chunkStats := s.calculateAccountStats(txs)
		newCursor := uint64(txs[len(txs)-1].ID)

		// ── 原子提交：snapshot 增量 upsert + 游标推进 必须在同一 MySQL 事务里 ──
		// UpsertBatchIncremental 使用 total_debit = total_debit + VALUES(...) 累加语义；
		// 若 upsert 成功但游标推进前崩溃，重跑同一 chunk → total_debit/credit 被加两遍 → 试算不平。
		// 两张表都在同一 db（dbIndex 路由），单 GORM Transaction 即可保证原子性。
		snapshots := make([]*model.AccountBalanceSnapshot, 0, len(chunkStats))
		for _, stat := range chunkStats {
			snapCurrency := currency
			if snapCurrency == "" {
				snapCurrency = "PHP"
			}
			snapshots = append(snapshots, &model.AccountBalanceSnapshot{
				AccountNo:         stat.AccountNo,
				SnapshotDate:      cutDate,
				RunID:             runID,
				BeginningBalance:  stat.BeginningBalance,
				EndingBalance:     stat.EndingBalance,
				TotalDebit:        stat.TotalDebit,
				TotalCredit:       stat.TotalCredit,
				TransactionCount:  stat.TransactionCount,
				Currency:          snapCurrency,
				LastTransactionID: stat.LastTransactionID,
			})
		}
		db, dbErr := s.dbManager.GetDB(dbIndex)
		if dbErr != nil {
			s.dayCutControlRepo.UpdateStatus(ctx, dbIndex, tableIndex, cutDate, runID, model.DayCutStatusFailed, dbErr.Error(), &startTime) //nolint:errcheck
			return fmt.Errorf("get db for chunk tx: %w", dbErr)
		}
		if txErr := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := s.balanceSnapshotRepo.UpsertBatchIncrementalTx(tx, tableIndex, snapshots); err != nil {
				return fmt.Errorf("upsert chunk %d snapshots: %w", chunkIdx, err)
			}
			if err := s.dayCutControlRepo.SetLastProcessedIDTx(tx, dbIndex, tableIndex, cutDate, runID, newCursor); err != nil {
				return fmt.Errorf("advance checkpoint to %d: %w", newCursor, err)
			}
			return nil
		}); txErr != nil {
			s.dayCutControlRepo.UpdateStatus(ctx, dbIndex, tableIndex, cutDate, runID, model.DayCutStatusFailed, txErr.Error(), &startTime) //nolint:errcheck
			return txErr
		}

		totalTxProcessed += len(txs)
		lastTransactionID = txs[len(txs)-1].TransactionID

		s.logger.Info("shard day cut chunk done",
			append(logBase,
				zap.Int("chunkIdx", chunkIdx),
				zap.Uint64("cursorBefore", cursor),
				zap.Uint64("cursorAfter", newCursor),
				zap.Int("txInChunk", len(txs)),
				zap.Int("accountsTouched", len(chunkStats)),
				zap.Duration("chunkDuration", time.Since(chunkStart)),
				zap.Int("totalTxSoFar", totalTxProcessed),
			)...)

		cursor = newCursor
		if len(txs) < dayCutChunkSize {
			break // 本 chunk 未填满 → 窗口已扫完
		}
	}

	// 6. Seal：用 canonical 公式覆盖所有账户的 beginning_balance / ending_balance。
	//    beginning = prev_cut.ending（首日 = 0）
	//    ending    = beginning + netDelta（asset/expense: DR-CR；其它: CR-DR）
	//    避免依赖 live account.Balance（buffered 账户跨 cut 写入会污染初期余额）。
	if err := s.sealRunSnapshots(ctx, dbIndex, tableIndex, cutDate, runID); err != nil {
		s.dayCutControlRepo.UpdateStatus(ctx, dbIndex, tableIndex, cutDate, runID, model.DayCutStatusFailed, err.Error(), &startTime) //nolint:errcheck
		return fmt.Errorf("seal snapshots: %w", err)
	}

	// 7. UpdateStatusCompleted
	if lastTransactionID == "" {
		lastTransactionID = "NONE"
	}
	if err := s.dayCutControlRepo.UpdateStatusCompleted(ctx, dbIndex, tableIndex, cutDate, runID, &startTime, lastTransactionID); err != nil {
		return err
	}
	s.logger.Info("shard day cut completed",
		append(logBase,
			zap.Int("totalChunks", chunkIdx),
			zap.Int("totalTxProcessed", totalTxProcessed),
			zap.String("lastTransactionID", lastTransactionID),
			zap.Duration("totalDuration", time.Since(startTime)),
		)...)
	return nil
}

// sealRunSnapshots 用 canonical 公式覆盖本 run 所有 snapshot 行的 beginning/ending。
//
// 公式（无论 sync 还是 buffered 账户都适用）：
//
//	beginning_today = prev_cut.ending  （首日 / 该账户首次 cut → 0）
//	ending_today    = beginning_today + netDelta
//	netDelta        = TotalDebit - TotalCredit       （asset/expense）
//	netDelta        = TotalCredit - TotalDebit       （liability/equity/revenue）
//
// 比起旧的 finalizeBufferedAccountsByCutDate，这里：
//   - 不再依赖 live account.Balance（buffered 账户的 account.Balance 包含跨 cut 的累
//     计写入；首日 cut 之前的 buffer flush 会污染本 run 初期余额，导致 fresh-DB
//     第一天的 beginning_balance ≠ 0）。
//   - 不再 query bufferRepo / outboxRepo（那些是为了"反推 ending"用的）。
//   - 同等对待 sync + buffered 账户，行为统一，trial balance 始终 SUM(beg)+SUM(net)=SUM(end)。
//
// 副作用：覆盖 chunk 阶段 UpsertBatchIncremental 写入的 beginning/ending（那些
// 来自 tx.BalanceBefore / tx.BalanceAfter，对 sync 账户准确，对 buffered 不准确）。
func (s *dayCutService) sealRunSnapshots(ctx context.Context, dbIndex, tableIndex int, cutDate string, runID int) error {
	rows, err := s.balanceSnapshotRepo.ListByCutAndRun(ctx, dbIndex, tableIndex, cutDate, runID)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	accountNos := make([]string, 0, len(rows))
	for _, r := range rows {
		accountNos = append(accountNos, r.AccountNo)
	}
	accounts, err := s.accountRepo.GetAccountsByNos(ctx, accountNos, dbIndex, tableIndex)
	if err != nil {
		return fmt.Errorf("batch get accounts: %w", err)
	}
	catMap := make(map[string]model.AccountCategory, len(accounts))
	for _, a := range accounts {
		catMap[a.AccountNo] = a.AccountCategory
	}

	begs := make(map[string]int64, len(rows))
	ends := make(map[string]int64, len(rows))
	for _, r := range rows {
		category, ok := catMap[r.AccountNo]
		if !ok {
			// snapshot 行存在但 account 表已不存在（极小概率：账户被删 + cleanup 未跑）
			// 跳过，不覆盖。后续如有需要可改为 fail。
			s.logger.Warn("sealRunSnapshots: account not found, skipping",
				zap.String("accountNo", r.AccountNo),
				zap.String("cutDate", cutDate),
				zap.Int("runID", runID))
			continue
		}
		prevEnd, _, err := s.balanceSnapshotRepo.GetPrevEnding(ctx, dbIndex, tableIndex, r.AccountNo, cutDate)
		if err != nil {
			return fmt.Errorf("get prev ending for %s: %w", r.AccountNo, err)
		}
		var netDelta int64
		if commonutil.IsAssetOrExpense(category) {
			netDelta = r.TotalDebit - r.TotalCredit
		} else {
			netDelta = r.TotalCredit - r.TotalDebit
		}
		begs[r.AccountNo] = prevEnd
		ends[r.AccountNo] = prevEnd + netDelta
	}

	if err := s.balanceSnapshotRepo.SetBeginAndEnd(ctx, dbIndex, tableIndex, cutDate, runID, begs, ends); err != nil {
		return fmt.Errorf("set begin/end: %w", err)
	}
	s.logger.Info("sealed run snapshots",
		zap.Int("dbIndex", dbIndex), zap.Int("tableIndex", tableIndex),
		zap.String("cutDate", cutDate), zap.Int("runID", runID),
		zap.Int("rows", len(rows)), zap.Int("sealed", len(begs)))
	return nil
}

// AccountStats 账户统计信息
type AccountStats struct {
	AccountNo        string
	TotalDebit       int64
	TotalCredit      int64
	TransactionCount int
	BeginningBalance int64
	EndingBalance    int64
	// LastTransactionID 该账户本日最后一笔流水ID（雪花ID自增单调）。
	LastTransactionID string
	// HasBufferedBooking 标记该账户本日是否存在缓冲记账流水。
	HasBufferedBooking bool
}

// calculateAccountStats 计算账户统计信息
func (s *dayCutService) calculateAccountStats(transactions []*model.AccountTransaction) map[string]*AccountStats {
	stats := make(map[string]*AccountStats)

	for _, tx := range transactions {
		stat, exists := stats[tx.AccountNo]
		if !exists {
			stat = &AccountStats{
				AccountNo:        tx.AccountNo,
				TotalDebit:       0,
				TotalCredit:      0,
				TransactionCount: 0,
			}
			stats[tx.AccountNo] = stat
		}

		stat.TotalDebit += tx.DebitAmount
		stat.TotalCredit += tx.CreditAmount
		stat.TransactionCount++

		if stat.TransactionCount == 1 {
			stat.BeginningBalance = tx.BalanceBefore
		}
		stat.EndingBalance = tx.BalanceAfter
		stat.LastTransactionID = tx.TransactionID

		if tx.BookingType == model.TransactionBookingTypeBuffered {
			stat.HasBufferedBooking = true
		}
	}

	return stats
}

// fixBufferedAccountBalances（已删除）：被 finalizeBufferedAccountsByWindow 替代。
// Plan B 切窗后，buffered 账户的 finalize 不再依赖内存里的 stats，而是从 DB 重新
// 按 finalized_at 时间窗拉账户号 + 读 snapshot。

// ListDayCutHistory returns historical day-cut entries grouped by (cut_date, run_id), sorted DESC.
func (s *dayCutService) ListDayCutHistory(ctx context.Context) ([]*DayCutHistoryEntry, error) {
	shards := s.router.GetAllShards()

	type shardRows struct {
		rows []repository.DayCutDateStatusRow
	}
	results := make([]shardRows, len(shards))

	// 并发查询所有分片（与 CheckDayCutStatus 相同的 fan-out 模式）
	var wg sync.WaitGroup
	for i, shard := range shards {
		i, shard := i, shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := s.dayCutControlRepo.ListAllStatuses(ctx, shard.DBIndex, shard.TableIndex)
			if err != nil {
				s.logger.Warn("ListDayCutHistory: shard query failed",
					zap.Int("dbIndex", shard.DBIndex),
					zap.Int("tableIndex", shard.TableIndex),
					zap.Error(err))
				return
			}
			results[i] = shardRows{rows: rows}
		}()
	}
	wg.Wait()

	// 聚合 key 加上 currency：同日多币种各跑一次时，按币种独立呈现。
	type entryKey struct {
		cutDate  string
		runID    int
		currency string
	}
	entriesMap := make(map[entryKey]*DayCutHistoryEntry)

	for _, res := range results {
		for _, row := range res.rows {
			k := entryKey{cutDate: row.CutDate, runID: row.RunID, currency: row.Currency}
			e, ok := entriesMap[k]
			if !ok {
				e = &DayCutHistoryEntry{CutDate: row.CutDate, RunID: row.RunID, Currency: row.Currency}
				entriesMap[k] = e
			}
			e.TotalShards += row.Count
			switch row.Status {
			case model.DayCutStatusPending:
				e.Pending += row.Count
			case model.DayCutStatusProcessing:
				e.Processing += row.Count
			case model.DayCutStatusCompleted:
				e.Completed += row.Count
			case model.DayCutStatusFailed:
				e.Failed += row.Count
			}
		}
	}

	entries := make([]*DayCutHistoryEntry, 0, len(entriesMap))
	for _, e := range entriesMap {
		entries = append(entries, e)
	}
	// Sort by cut_date DESC, run_id DESC
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CutDate != entries[j].CutDate {
			return entries[i].CutDate > entries[j].CutDate
		}
		return entries[i].RunID > entries[j].RunID
	})
	return entries, nil
}

// CheckDayCutStatus 检查日切状态（并行扫描所有分片后汇总统计）
func (s *dayCutService) CheckDayCutStatus(ctx context.Context, cutDate string) (map[string]interface{}, error) {
	shards := s.router.GetAllShards()

	type shardResult struct {
		summary map[int8]int
		err     error
	}
	results := make([]shardResult, len(shards))

	// 并发查询所有分片（受 Go runtime 协程调度控制，实际并发 ≈ GOMAXPROCS）
	var wg sync.WaitGroup
	for i, shard := range shards {
		i, shard := i, shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			summary, err := s.dayCutControlRepo.QueryStatusSummary(ctx, shard.DBIndex, shard.TableIndex, cutDate)
			results[i] = shardResult{summary: summary, err: err}
		}()
	}
	wg.Wait()

	statusTotals := make(map[int8]int)
	for i, res := range results {
		if res.err != nil {
			shard := shards[i]
			return nil, fmt.Errorf("query day cut status failed for shard(%d,%d): %w", shard.DBIndex, shard.TableIndex, res.err)
		}
		for status, count := range res.summary {
			statusTotals[status] += count
		}
	}

	statusMap := map[string]interface{}{"cut_date": cutDate}
	for status, count := range statusTotals {
		switch status {
		case model.DayCutStatusPending:
			statusMap["pending"] = count
		case model.DayCutStatusProcessing:
			statusMap["processing"] = count
		case model.DayCutStatusCompleted:
			statusMap["completed"] = count
		case model.DayCutStatusFailed:
			statusMap["failed"] = count
		}
	}

	return statusMap, nil
}

// GetLatestCompletedRunID delegates to the repository.
func (s *dayCutService) GetLatestCompletedRunID(ctx context.Context, cutDate string) (int, bool, error) {
	return s.dayCutControlRepo.GetLatestCompletedRunID(ctx, cutDate)
}

// WatchdogRecover scans today's day-cut shards and re-launches any shard stuck in
// PENDING or PROCESSING for longer than stuckThreshold. Each recovery runs in its own
// goroutine with a 30-minute independent timeout so it never blocks the watchdog tick.
//
// 并行化：所有分片的 GetMaxRunID + GetByShard 并发执行，减少检测延迟从
// O(100 × DB latency) → O(max(DB latency))。
func (s *dayCutService) WatchdogRecover(ctx context.Context, stuckThreshold time.Duration) error {
	today := time.Now().Format("2006-01-02")
	stuckBefore := time.Now().Add(-stuckThreshold)
	shards := s.router.GetAllShards()

	type shardCheck struct {
		maxRunID int
		ctrl     *model.DayCutControl
	}
	checks := make([]shardCheck, len(shards))

	// 并发查询所有分片的状态
	var wg sync.WaitGroup
	for i, shard := range shards {
		i, shard := i, shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			maxRunID, err := s.dayCutControlRepo.GetMaxRunID(ctx, shard.DBIndex, shard.TableIndex, today)
			if err != nil {
				s.logger.Warn("watchdog: GetMaxRunID failed",
					zap.Int("dbIndex", shard.DBIndex),
					zap.Int("tableIndex", shard.TableIndex),
					zap.Error(err))
				return
			}
			if maxRunID == 0 {
				return
			}
			ctrl, err := s.dayCutControlRepo.GetByShard(ctx, shard.DBIndex, shard.TableIndex, today, maxRunID)
			if err != nil {
				s.logger.Warn("watchdog: GetByShard failed",
					zap.Int("dbIndex", shard.DBIndex),
					zap.Int("tableIndex", shard.TableIndex),
					zap.Error(err))
				return
			}
			checks[i] = shardCheck{maxRunID: maxRunID, ctrl: ctrl}
		}()
	}
	wg.Wait()

	// 串行筛选 → 批量限流并发恢复。
	var stuckShards []stuckShard
	for i, shard := range shards {
		check := checks[i]
		if check.ctrl == nil {
			continue
		}
		if check.ctrl.Status == model.DayCutStatusCompleted {
			continue
		}
		stuck := check.ctrl.Status == model.DayCutStatusPending || check.ctrl.Status == model.DayCutStatusProcessing
		if !stuck || check.ctrl.UpdatedAt.After(stuckBefore) {
			continue
		}
		_ = shard // shard info copied into stuckShard below for clarity

		stuckShards = append(stuckShards, stuckShard{
			dbIdx:    shard.DBIndex,
			tableIdx: shard.TableIndex,
			runID:    check.maxRunID,
			status:   check.ctrl.Status,
			updated:  check.ctrl.UpdatedAt,
			currency: check.ctrl.Currency,
		})
	}

	// 恢复并发度限制：100 分片同时 stuck 时，若无限扇出 goroutine，
	// 每个都要扫百万级流水 → DB 连接池 / CPU / 锁 buffer 全部爆炸、进程 OOM → 循环崩溃。
	// 用 semaphore 把并发限制在 watchdogRecoveryConcurrency，其余排队。
	metrics.WatchdogStuckShardsGauge.Set(float64(len(stuckShards)))
	if len(stuckShards) == 0 {
		return nil
	}
	sem := make(chan struct{}, watchdogRecoveryConcurrency)
	for _, ss := range stuckShards {
		ss := ss
		s.logger.Warn("watchdog: stuck shard detected, recovering (bounded)",
			zap.Int("dbIndex", ss.dbIdx),
			zap.Int("tableIndex", ss.tableIdx),
			zap.String("cutDate", today),
			zap.Int("runID", ss.runID),
			zap.Int8("status", ss.status),
			zap.Time("lastUpdated", ss.updated),
			zap.Int("semCapacity", watchdogRecoveryConcurrency),
		)
		sem <- struct{}{} // 队列满则阻塞，天然限流；不用 goroutine + channel 往外推积压
		metrics.WatchdogRecoveriesTriggeredTotal.Inc()
		go func() {
			defer func() { <-sem }()
			// 派生自调用方 ctx 而非 Background：保留 trace/tag/metadata 但脱离 HTTP 取消（WithoutCancel）。
			bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dayCutShardTimeout)
			defer cancel()
			if err := s.ProcessShardDayCut(bgCtx, ss.dbIdx, ss.tableIdx, today, ss.runID, ss.currency); err != nil {
				s.logger.Error("watchdog: shard recovery failed",
					zap.Int("dbIndex", ss.dbIdx),
					zap.Int("tableIndex", ss.tableIdx),
					zap.String("cutDate", today),
					zap.Int("runID", ss.runID),
					zap.String("currency", ss.currency),
					zap.Error(err))
			}
		}()
	}

	return nil
}

// ResumeStuckShards 见接口注释。跨所有分片扫描指定 (cutDate, runID) 的卡死 PROCESSING
// 行，重新派发 ProcessShardDayCut。stuckThreshold = 0 时强制恢复全部 PROCESSING。
//
// 实现要点：
//   - ListStuckProcessing 在每个分片本地扫描（按 dbIndex/tableIndex 路由）
//   - 用 dayCutShardTimeout 包 ctx，避免新派发的 goroutine 永远不退出
//   - 同样用 watchdogRecoveryConcurrency 限制并发，防止"100 分片同时恢复"打爆 DB
func (s *dayCutService) ResumeStuckShards(ctx context.Context, cutDate string, runID int, stuckThreshold time.Duration) (int, error) {
	cutoff := time.Now()
	if stuckThreshold > 0 {
		cutoff = cutoff.Add(-stuckThreshold)
	} else {
		// stuckThreshold = 0 → 强制：cutoff 设到未来一天，匹配所有 updated_at < ?
		cutoff = cutoff.Add(24 * time.Hour)
	}

	shards := s.router.GetAllShards()
	type stuckEntry struct {
		dbIdx, tableIdx int
		runID           int
		currency        string // 初次触发时的币种过滤，恢复时透传
	}
	var stuck []stuckEntry
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, shard := range shards {
		shard := shard
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := s.dayCutControlRepo.ListStuckProcessing(ctx, shard.DBIndex, shard.TableIndex, cutoff)
			if err != nil {
				s.logger.Warn("resume: ListStuckProcessing failed",
					zap.Int("dbIndex", shard.DBIndex),
					zap.Int("tableIndex", shard.TableIndex),
					zap.Error(err))
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows {
				if row.CutDate == cutDate && row.RunID == runID {
					stuck = append(stuck, stuckEntry{
						dbIdx:    shard.DBIndex,
						tableIdx: shard.TableIndex,
						runID:    row.RunID,
						currency: row.Currency,
					})
				}
			}
		}()
	}
	wg.Wait()

	if len(stuck) == 0 {
		s.logger.Info("resume: no stuck shards found",
			zap.String("cutDate", cutDate),
			zap.Int("runID", runID),
			zap.Duration("stuckThreshold", stuckThreshold))
		return 0, nil
	}

	s.logger.Warn("resume: re-dispatching stuck shards",
		zap.String("cutDate", cutDate),
		zap.Int("runID", runID),
		zap.Int("count", len(stuck)))

	// 派发 goroutine 用独立 ctx（脱离 admin HTTP 请求 ctx），但保留 deadline 兜底。
	// stuck shards 数量通常 < 100，bounded by watchdogRecoveryConcurrency 即可。
	bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dayCutShardTimeout)
	sem := make(chan struct{}, watchdogRecoveryConcurrency)
	go func() {
		// 等所有 worker 退出再 cancel，否则在途任务被截断
		var dispatchWG sync.WaitGroup
		for _, ss := range stuck {
			ss := ss
			sem <- struct{}{}
			dispatchWG.Add(1)
			metrics.WatchdogRecoveriesTriggeredTotal.Inc()
			go func() {
				defer func() { <-sem; dispatchWG.Done() }()
				if err := s.ProcessShardDayCut(bgCtx, ss.dbIdx, ss.tableIdx, cutDate, ss.runID, ss.currency); err != nil {
					s.logger.Error("resume: shard process failed",
						zap.Int("dbIndex", ss.dbIdx),
						zap.Int("tableIndex", ss.tableIdx),
						zap.String("cutDate", cutDate),
						zap.Int("runID", ss.runID),
						zap.Error(err))
				}
			}()
		}
		dispatchWG.Wait()
		cancel()
		s.logger.Info("resume: all stuck shards finished re-dispatch",
			zap.String("cutDate", cutDate),
			zap.Int("runID", runID),
			zap.Int("count", len(stuck)))
	}()
	return len(stuck), nil
}

// dayCutShardTimeout 单个分片日切运行的硬超时。
// 30min 是经验值：正常每分片 batched + checkpoint 几分钟内完成；卡住的分片由 watchdog
// 下一个 tick 重新拾起（新 runID）继续。4h 是老值，太宽松会掩盖问题。
const dayCutShardTimeout = 30 * time.Minute

// watchdogRecoveryConcurrency 限制同一次 WatchdogRecover tick 中并发恢复的分片数。
// 5 是经验值：即便 100 分片全部 stuck，也只有 5 个同时跑日切——
// 每个 ProcessShardDayCut 的 batch 循环已经占用 DB 连接池若干槽位，5 路并发能在
// DB 压力可控前提下 20 分钟内依次处理完毕。若观测到 watchdog 始终清不完积压，
// 再上调（需一并提升连接池上限）。
const watchdogRecoveryConcurrency = 5

// stuckShard 内部结构：在 watchdog 串行判断阶段收集起来，
// 然后统一用 semaphore 限流后再并发恢复。
type stuckShard struct {
	dbIdx    int
	tableIdx int
	runID    int
	status   int8
	updated  time.Time
	// currency 记录初次触发日切时的过滤，恢复时回传保证行为一致。
	// 老 day_cut_control 行没有 currency 列 / 值为 "" 时等价于"全部币种"。
	currency string
}

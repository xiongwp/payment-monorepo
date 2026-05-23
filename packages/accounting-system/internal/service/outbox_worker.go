package service

// OutboxWorker 结算 Outbox 工作器
//
// 职责：
//   MySQL Writer  ─ 每 100ms 批量消费 REDIS_DONE 记录，持久化到 MySQL
//                   （写 account_transaction 流水 + 更新 account 余额），然后置 MYSQL_DONE。
//   Recovery      ─ 每 10s 扫描超过 30s 仍处于 PENDING 的记录（进程崩溃场景）。
//                   对每条记录重试 Redis Lua 转账；若账户已不在缓存则直接置 REDIS_DONE
//                   （OutboxWorker 会凭 event_data 从 MySQL 恢复正确余额）。
//                   重试 5 次仍失败则置 FAILED 并打 Fatal 告警（需人工介入）。
//
// 幂等性：
//   account_transaction.transaction_id 有唯一索引，重复写入直接跳过。
//   account 余额更新覆盖写，天然幂等（最终与 Redis 对齐）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
	"github.com/xiongwp/accounting-system/internal/infrastructure/cache"
	"github.com/xiongwp/accounting-system/internal/infrastructure/database"
	kafkamq "github.com/xiongwp/accounting-system/internal/infrastructure/kafka"
	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
	"github.com/xiongwp/accounting-system/internal/metrics"
	"github.com/xiongwp/accounting-system/internal/repository"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// OutboxWorker 结算 Outbox 后台工作器
type OutboxWorker struct {
	outboxRepo      repository.SettlementOutboxRepository
	orderRepo       repository.TransactionOrderRepository
	transactionRepo repository.TransactionRepository
	dbManager       *database.Manager
	router          *sharding.Router
	balanceCache    *cache.BalanceCache
	cutDate         CutDateProvider // 计算 entry.cut_date；nil 时回退到 transaction_date
	logger          *zap.Logger
	wg              sync.WaitGroup // 跟踪所有常驻 goroutine，让 fx OnStop 能等它们 drain
}

// NewOutboxWorker 创建 OutboxWorker
func NewOutboxWorker(
	outboxRepo repository.SettlementOutboxRepository,
	orderRepo repository.TransactionOrderRepository,
	transactionRepo repository.TransactionRepository,
	dbManager *database.Manager,
	router *sharding.Router,
	balanceCache *cache.BalanceCache,
	cutDate CutDateProvider,
	logger *zap.Logger,
) *OutboxWorker {
	return &OutboxWorker{
		outboxRepo:      outboxRepo,
		orderRepo:       orderRepo,
		transactionRepo: transactionRepo,
		dbManager:       dbManager,
		router:          router,
		balanceCache:    balanceCache,
		cutDate:         cutDate,
		logger:          logger,
	}
}

// Wait 阻塞等待 Start 启动的所有 goroutine 退出。
// 用法：fx OnStop 里 cancel() 后调 Wait()，保证不要在 goroutine 还在写 MySQL 时
// 关闭 DB 句柄；外层应给 deadline 兜底。
func (w *OutboxWorker) Wait() {
	w.wg.Wait()
}

// Start 启动工作器（四个常驻 goroutine）
//
//   - MySQLWriter    每 100ms 消费 REDIS_DONE outbox → 持久化到 MySQL
//   - OutboxRecovery 每 10s  恢复卡在 PENDING 的 outbox（进程崩溃场景）
//   - OrderRecovery  每 30s  恢复卡在 PROCESSING 的 TransactionOrder（进程崩溃场景）
//   - Cleanup        每 1h   删除超过 7 天的 MYSQL_DONE 记录（防止表无限增长）
func (w *OutboxWorker) Start(ctx context.Context) {
	w.wg.Add(4)
	go func() { defer w.wg.Done(); w.runMySQLWriter(ctx) }()
	go func() { defer w.wg.Done(); w.runRecovery(ctx) }()
	go func() { defer w.wg.Done(); w.runOrderRecovery(ctx) }()
	go func() { defer w.wg.Done(); w.runCleanup(ctx) }()
}

// StartKafkaConsumer 可选启动 Kafka outbox-notify 消费者（PERF-5 完整版）。
//
// 订阅 hot path 成功后推送的通知消息（key = voucher_no），收到即从 MySQL 拉取
// 该条 outbox 做 persist；polling 保留作为兜底，保证消息丢失 / 消费者重启 / 消费组
// offset 重置等场景下记录仍会被处理。
//
// 若 kafka.brokers 为空或 outbox_push.enabled=false，不调用此方法即可；worker 维持
// 纯轮询模式，行为与之前完全一致。
func (w *OutboxWorker) StartKafkaConsumer(ctx context.Context, cfg kafkamq.ConsumerConfig) {
	consumer := kafkamq.NewConsumer(cfg, func(ctx context.Context, msg kafkamq.Message) error {
		// msg.Key 为 voucher_no；从 MySQL 拉一条该 voucher 的 REDIS_DONE outbox 并立即处理。
		// 若此时该记录已被 polling 消费完（status=MYSQL_DONE），本次 no-op 返回。
		voucherNo, _ := msg.Data.(string)
		if voucherNo == "" {
			voucherNo = msg.Key
		}
		if voucherNo == "" {
			return nil // 空消息忽略，consumer 仍 commit offset
		}
		rec, err := w.outboxRepo.FindByVoucher(ctx, voucherNo)
		if err != nil || rec == nil || rec.Status != model.OutboxStatusRedisDone {
			return nil // 已被其他 goroutine / polling 处理
		}
		if err := w.processRecords(ctx, []*model.SettlementOutbox{rec}); err != nil {
			w.logger.Warn("outbox kafka-notify processing failed, will be picked up by polling",
				zap.String("voucherNo", voucherNo), zap.Error(err))
		}
		return nil // 无论结果都 ack，避免 Kafka 重投阻塞进度；失败由 polling + FAILED 兜底
	}, w.logger)

	go func() {
		if err := consumer.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("outbox kafka consumer stopped", zap.Error(err))
		}
	}()
	w.logger.Info("outbox kafka notify consumer started",
		zap.String("topic", cfg.Topic),
		zap.String("groupID", cfg.GroupID))
}

// ─── MySQL Writer ─────────────────────────────────────────────────────────────

func (w *OutboxWorker) runMySQLWriter(ctx context.Context) {
	// 自适应轮询：满批时以 mysqlWriterInterval 快速继续扫，空批时指数退避到最多
	// mysqlWriterIdleMax，降低空载时的 100Hz × 分片数 纯轮询压力。
	// 有记录时立即下一轮（只 sleep mysqlWriterInterval），保证延迟特征不变。
	delay := mysqlWriterInterval
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		processed, err := w.processBatchReturning(ctx)
		if err != nil {
			w.logger.Error("outbox mysql writer error", zap.Error(err))
		}
		if processed > 0 {
			// 有活干，保持快速循环
			delay = mysqlWriterInterval
		} else {
			// 空批，退避（×2，封顶）
			delay *= 2
			if delay > mysqlWriterIdleMax {
				delay = mysqlWriterIdleMax
			}
		}
		// 可被 ctx 取消的 sleep
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// processBatchReturning 跟 processBatch 语义相同，额外返回本轮处理条数（用于自适应退避）。
func (w *OutboxWorker) processBatchReturning(ctx context.Context) (int, error) {
	records, err := w.outboxRepo.FindByStatus(ctx, model.OutboxStatusRedisDone, 500)
	if err != nil {
		return 0, fmt.Errorf("find redis_done: %w", err)
	}
	metrics.OutboxPendingGauge.Set(float64(len(records)))
	// 更新 oldest pending age：拿队列中 created_at 最早的算 lag，没积压时归零。
	// FindByStatus 默认按 created_at ASC，第一条就是最老的。
	if len(records) == 0 {
		metrics.OutboxOldestPendingAgeSeconds.Set(0)
		return 0, nil
	}
	oldestAge := time.Since(records[0].CreatedAt).Seconds()
	if oldestAge < 0 {
		oldestAge = 0
	}
	metrics.OutboxOldestPendingAgeSeconds.Set(oldestAge)
	if err := w.processRecords(ctx, records); err != nil {
		return len(records), err
	}
	return len(records), nil
}

const (
	maxMySQLWriteRetries  = 5             // REDIS_DONE 记录 MySQL 写入失败时的最大重试次数
	maxRecoveryRetries    = 5             // PENDING 记录 Redis 重试的最大次数
	mysqlWriterInterval   = 100 * time.Millisecond
	// mysqlWriterIdleMax 空批时的最大退避。
	// 100 分片 × 10 Hz = 1000 QPS 纯轮询；空载时逐步退到 2s，
	// 载时立即回到 100ms，不影响 p99 延迟。
	mysqlWriterIdleMax = 2 * time.Second
	outboxRecoveryInterval = 10 * time.Second
	// 压测 tuning（优化 C）：原 30s 扫 + 60s 阈值在 50 并发下偶发误伤 —— confirm
	// 阶段 mysql 慢一点就被标 FAILED。调到 120s 扫 + 300s 阈值，给真实慢请求容忍。
	// 生产环境恢复 30s/60s 是合理的（业务期望快速恢复 stuck order）。
	orderRecoveryInterval  = 120 * time.Second
	cleanupInterval        = 1 * time.Hour
	stuckPendingThreshold  = 30 * time.Second  // PENDING 超过此时间视为卡住
	stuckProcessingThreshold = 300 * time.Second // PROCESSING 超过此时间视为卡住（压测 tuning）
	recoveryQueryTimeout   = 5 * time.Second  // 每次 recovery DB 查询的超时时间
	cleanupQueryTimeout    = 5 * time.Second  // 每次 cleanup DB 查询的超时时间
	retryBaseDelay         = 100 * time.Millisecond // 指数退避基础延迟
)

// processBatch 查询 + 处理一批 REDIS_DONE outbox；保留旧签名供 test / 其他调用方使用。
func (w *OutboxWorker) processBatch(ctx context.Context) error {
	_, err := w.processBatchReturning(ctx)
	return err
}

// processRecords 并行处理一批已查询出来的 REDIS_DONE outbox 记录。
func (w *OutboxWorker) processRecords(ctx context.Context, records []*model.SettlementOutbox) error {
	var wg sync.WaitGroup
	for _, rec := range records {
		rec := rec
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.persistToMySQL(ctx, rec); err != nil {
				w.logger.Error("outbox: persist to mysql failed",
					zap.String("voucherNo", rec.VoucherNo),
					zap.Int("retryCount", rec.RetryCount),
					zap.Error(err))

				if rec.RetryCount >= maxMySQLWriteRetries {
					_ = w.outboxRepo.MarkFailed(ctx, rec.VoucherNo, truncateErrMsg(err.Error()))
					metrics.OutboxFailedTotal.Inc()
					w.logger.Error("CRITICAL: outbox REDIS_DONE record permanently failed, manual intervention required",
						zap.String("voucherNo", rec.VoucherNo),
						zap.Int("retryCount", rec.RetryCount))
				} else {
					_ = w.outboxRepo.IncrementRetry(ctx, rec.VoucherNo, truncateErrMsg(err.Error()))
				}
			}
		}()
	}
	wg.Wait()
	return nil
}

// persistToMySQL 将一条 REDIS_DONE outbox 持久化到 MySQL（幂等）
func (w *OutboxWorker) persistToMySQL(ctx context.Context, rec *model.SettlementOutbox) error {
	var event model.SettlementEvent
	if err := json.Unmarshal([]byte(rec.EventData), &event); err != nil {
		// 数据格式错误：直接 FAILED，不重试
		_ = w.outboxRepo.MarkFailed(ctx, rec.VoucherNo, "unmarshal event: "+err.Error())
		return fmt.Errorf("unmarshal outbox event(%s): %w", rec.VoucherNo, err)
	}

	// **资金安全核心**：cut_date 严格读 outbox 行持久化值（入口落库时一次定死），
	// 永不在本 worker 重算 —— 抗时钟漂移、抗多 pod、抗 SystemConfig 变更、抗 DB
	// 重启。空字符串兜底走 transaction_date（仅老数据；新数据一律有 cut_date）。
	cutDate := rec.CutDate
	if cutDate == "" {
		cutDate = rec.TransactionDate
		if cutDate == "" {
			cutDate = event.TransactAt.Format("2006-01-02")
		}
		w.logger.Warn("outbox row missing cut_date, falling back (legacy data)",
			zap.String("voucherNo", rec.VoucherNo),
			zap.String("fallback", cutDate))
	}

	for _, entry := range event.Entries {
		start := time.Now()
		if err := w.persistEntry(ctx, &event, &entry, cutDate); err != nil {
			w.logger.Error("outbox: persist entry failed",
				zap.String("voucherNo", rec.VoucherNo),
				zap.String("accountNo", entry.AccountNo),
				zap.String("txID", entry.TransactionID),
				zap.Error(err))
			return err
		}
		metrics.OutboxMySQLWriteDuration.Observe(time.Since(start).Seconds())
	}

	if err := w.outboxRepo.MarkMySQLDone(ctx, rec.VoucherNo); err != nil {
		return err
	}
	metrics.OutboxProcessedTotal.Inc()
	return nil
}

func (w *OutboxWorker) persistEntry(ctx context.Context, event *model.SettlementEvent, entry *model.SettlementEntry, cutDate string) error {
	dbIdx, tableIdx := w.router.RouteByAccountNo(entry.AccountNo)
	db, err := w.dbManager.GetDB(dbIdx)
	if err != nil {
		return fmt.Errorf("get db[%d]: %w", dbIdx, err)
	}

	balanceBefore, err := strconv.ParseInt(entry.BalanceBefore, 10, 64)
	if err != nil {
		return fmt.Errorf("parse balance_before(%s): %w", entry.AccountNo, err)
	}
	balanceAfter, err := strconv.ParseInt(entry.BalanceAfter, 10, 64)
	if err != nil {
		return fmt.Errorf("parse balance_after(%s): %w", entry.AccountNo, err)
	}
	debit, err := strconv.ParseInt(entry.DebitAmount, 10, 64)
	if err != nil {
		return fmt.Errorf("parse debit_amount(%s): %w", entry.AccountNo, err)
	}
	credit, err := strconv.ParseInt(entry.CreditAmount, 10, 64)
	if err != nil {
		return fmt.Errorf("parse credit_amount(%s): %w", entry.AccountNo, err)
	}
	desc := event.Description

	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return fmt.Errorf("begin tx: %w", tx.Error)
	}
	defer func() {
		if rbErr := tx.Rollback().Error; rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			w.logger.Warn("outbox: rollback failed", zap.String("txID", entry.TransactionID), zap.Error(rbErr))
		}
	}()

	// 写流水（幂等：transaction_id 唯一索引冲突时跳过）。
	//
	// cut_date 来自调用方传入的 outbox 行持久化值（入口落库那一刻定死，永不重算），
	// 抗时钟漂移、抗 SystemConfig 变更、抗 DB 重启、抗多 pod replay 时序差异。
	transaction := &model.AccountTransaction{
		TransactionID:       entry.TransactionID,
		ParentTransactionID: &event.VoucherNo,
		AccountNo:           entry.AccountNo,
		BusinessNo:          event.BusinessNo,
		BusinessType:        event.BusinessType,
		DebitAmount:         debit,
		CreditAmount:        credit,
		BalanceBefore:       balanceBefore,
		BalanceAfter:        balanceAfter,
		BookingType:         model.TransactionBookingTypeBuffered,
		Currency:            event.Currency,
		TransactionDate:     event.TransactAt.Format("2006-01-02"),
		TransactionTime:     event.TransactAt,
		Description:         &desc,
		Status:              model.TransactionStatusSuccess,
		CutDate:             cutDate,
	}
	if err := w.transactionRepo.CreateTransaction(ctx, tx, transaction, dbIdx, tableIdx); err != nil {
		if isDuplicateKeyError(err) {
			w.logger.Debug("outbox entry already persisted, skipping",
				zap.String("txID", entry.TransactionID))
			tx.Rollback() //nolint:errcheck
			return nil
		}
		return fmt.Errorf("create transaction(%s): %w", entry.TransactionID, err)
	}

	// 更新 MySQL account 余额（增量写，保证并发安全与幂等性）。
	//
	// 为什么用增量（balance += delta）而不是覆盖（SET balance = newValue）：
	//   - 同一账户的多条 outbox 可能被不同 goroutine 并发处理
	//   - 覆盖写会因乱序导致余额被旧值覆盖（ABA 问题）
	//   - 增量写只要各 delta 正确，无论顺序，最终余额一定正确
	//
	// 幂等保证：
	//   - 上方 CreateTransaction（transaction_id 唯一索引）失败时提前返回，不会到达此行
	//   - 故同一条 entry 的增量更新只执行一次
	delta := balanceAfter - balanceBefore
	accountTable := w.router.GetTableName("account", tableIdx)
	result := tx.Table(accountTable).
		Where("account_no = ?", entry.AccountNo).
		Updates(map[string]interface{}{
			"balance":           gorm.Expr("balance + ?", delta),
			"available_balance": gorm.Expr("available_balance + ?", delta),
			"updated_at":        time.Now(),
		})
	if result.Error != nil {
		return fmt.Errorf("update account balance(%s): %w", entry.AccountNo, result.Error)
	}
	if result.RowsAffected == 0 {
		// 账户不存在：**绝不 MarkFailed** —— MarkFailed + return nil 会让上层 for
		// 循环误以为本 entry 处理完毕继续推进下一条 entry，造成 voucher 单边写入：
		// entry[0] 因账户缺失被 MarkFailed 跳过，entry[1] 落库成功 commit balance →
		// 凭证只有 entry[1] 的一半，借贷天然不平 = 资金错乱。
		//
		// 改为：return error 让 outbox 留 REDIS_DONE，下一轮 recovery 持续重试。
		// 账户最终被创建后自然成功（典型场景：上游 booking 之前刚 CreateAccount，
		// 在压测/DB 重启场景下可能尚未 commit 到本 worker 看到）。如果账户永远
		// 不存在，则 retry_count 累积，由 metrics CRITICAL 告警人工介入。
		tx.Rollback() //nolint:errcheck
		w.logger.Error("CRITICAL: outbox persistEntry — account not found, will retry indefinitely (do NOT MarkFailed)",
			zap.String("voucherNo", event.VoucherNo),
			zap.String("txID", entry.TransactionID),
			zap.String("accountNo", entry.AccountNo),
			zap.Int64("delta", delta),
		)
		metrics.OutboxFailedTotal.Inc()
		return fmt.Errorf("account %s not found in shard db=%d tbl=%d (will retry; do NOT mark FAILED — would cause partial-commit voucher)",
			entry.AccountNo, dbIdx, tableIdx)
	}

	return tx.Commit().Error
}

// ─── Recovery ────────────────────────────────────────────────────────────────

func (w *OutboxWorker) runRecovery(ctx context.Context) {
	ticker := time.NewTicker(outboxRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// trace.NewBackground：每次扫一拨 stuck PENDING 都新 trace_id +
			// shadow=false 强制。outbox 写主账表，绝不能漏带 shadow flag。
			tickCtx, cancel := trace.NewBackground(ctx, "outbox-recovery", w.logger, outboxRecoveryInterval)
			if err := w.recoverStuckPending(tickCtx); err != nil {
				trace.Logger(tickCtx, w.logger).Error("outbox recovery error", zap.Error(err))
			}
			cancel()
		}
	}
}

// recoverStuckPending 处理超过 stuckPendingThreshold 仍为 PENDING 的记录（进程崩溃场景）
//
// PENDING 超时说明服务在 Redis 更新之前崩溃了（outbox 已写，Redis 未更新）。
// 先自增 retry_count（CAS），再重试 Redis Lua 转账；
// 若账户已清出缓存则跳过 Redis（直接 REDIS_DONE），由 MySQL Writer 凭 event_data 恢复。
func (w *OutboxWorker) recoverStuckPending(ctx context.Context) error {
	qCtx, cancel := context.WithTimeout(ctx, recoveryQueryTimeout)
	defer cancel()
	records, err := w.outboxRepo.FindStuckPending(qCtx, stuckPendingThreshold, 50)
	if err != nil {
		return fmt.Errorf("find stuck pending: %w", err)
	}
	for _, rec := range records {
		// 先自增 retry_count，再执行重试。
		// IncrementRetry 执行 UPDATE ... SET retry_count = retry_count + 1 WHERE voucher_no = ? AND status = 'PENDING'
		// 若多实例并发扫到同一记录，只有一个实例能成功（其余因状态已变或 CAS 失败被跳过），
		// 避免同一记录被重复处理。
		if incErr := w.outboxRepo.IncrementRetry(ctx, rec.VoucherNo, "recovery attempt"); incErr != nil {
			w.logger.Warn("outbox recovery: increment retry failed, skipping",
				zap.String("voucherNo", rec.VoucherNo), zap.Error(incErr))
			continue // 其他实例已处理或记录状态已变，跳过
		}
		newCount := rec.RetryCount + 1 // 本实例视角的新重试次数（乐观估算，不影响正确性）

		if err := w.retryRedis(ctx, rec); err != nil {
			w.logger.Error("outbox recovery: redis retry failed",
				zap.String("voucherNo", rec.VoucherNo),
				zap.Int("retryCount", newCount),
				zap.Error(err))
			if newCount >= maxRecoveryRetries {
				_ = w.outboxRepo.MarkFailed(ctx, rec.VoucherNo, truncateErrMsg(err.Error()))
				metrics.OutboxFailedTotal.Inc()
				metrics.RecoveryFailuresTotal.WithLabelValues("outbox_pending").Inc()
				w.logger.Error("CRITICAL: outbox recovery permanently failed, manual intervention required",
					zap.String("voucherNo", rec.VoucherNo),
					zap.Int("retryCount", newCount))
			}
		} else {
			metrics.RecoveryTotal.WithLabelValues("outbox_pending").Inc()
		}
	}
	return nil
}

// retryRedis 对 PENDING outbox 重试 Redis Lua 转账
func (w *OutboxWorker) retryRedis(ctx context.Context, rec *model.SettlementOutbox) error {
	var event model.SettlementEvent
	if err := json.Unmarshal([]byte(rec.EventData), &event); err != nil {
		return fmt.Errorf("unmarshal event: %w", err)
	}

	deltaMap := make(map[string]string, len(event.Entries))
	for _, entry := range event.Entries {
		deltaMap[entry.AccountNo] = entry.BalanceDelta
	}

	// 传 voucherNo 触发 Lua 幂等保护：上次崩溃后 Redis 可能已经 HINCRBY 过，
	// 这里再次 Transfer 会走 already_applied 分支（SET NX 命中），不会 double-count。
	if err := w.balanceCache.Transfer(ctx, rec.VoucherNo, deltaMap); err != nil {
		// 账户已不在缓存：Redis 本轮不需要更新，直接进入 MySQL 落库阶段
		if isNotInCacheErr(err) {
			w.logger.Info("outbox recovery: accounts not in cache, skip redis, proceed to mysql write",
				zap.String("voucherNo", rec.VoucherNo))
			return w.outboxRepo.MarkRedisDone(ctx, rec.VoucherNo)
		}
		_ = w.outboxRepo.MarkFailed(ctx, rec.VoucherNo, err.Error())
		return err
	}

	return w.outboxRepo.MarkRedisDone(ctx, rec.VoucherNo)
}

// isNotInCacheErr 判断是否为"账户不在缓存"错误
func isNotInCacheErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "account not found")
}

// ─── Order Recovery ───────────────────────────────────────────────────────────

// runOrderRecovery 每 30s 扫描卡在 PROCESSING 超过 60s 的 TransactionOrder，重置为 FAILED
//
// 场景：服务进程在 UpdateToProcessing 之后、executeAndRecord 完成之前崩溃。
// 这类订单永远不会自行完成，调用方会一直收到 ErrRequestInProgress。
// 重置为 FAILED 后，调用方下次重试时可重新抢锁执行（幂等保护已就位）。
func (w *OutboxWorker) runOrderRecovery(ctx context.Context) {
	ticker := time.NewTicker(orderRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 每 tick 起 background ctx：trace_id 新生成 + shadow=false 强制。
			tickCtx, cancel := trace.NewBackground(ctx, "order-recovery", w.logger, recoveryQueryTimeout)
			affected, err := w.orderRepo.ResetStuckProcessing(tickCtx, stuckProcessingThreshold)
			cancel()
			if err != nil {
				trace.Logger(tickCtx, w.logger).Error("order recovery: reset stuck processing failed", zap.Error(err))
				continue
			}
			if affected > 0 {
				metrics.RecoveryTotal.WithLabelValues("order_processing").Add(float64(affected))
				trace.Logger(tickCtx, w.logger).Warn("order recovery: reset stuck PROCESSING orders to FAILED",
					zap.Int64("count", affected))
			}
		}
	}
}

// ─── Cleanup ──────────────────────────────────────────────────────────────────

// runCleanup 每小时删除 7 天以前的 MYSQL_DONE 记录，防止 settlement_outbox 无限增长。
// 保留 7 天的 MYSQL_DONE 记录用于人工审计和问题排查。
func (w *OutboxWorker) runCleanup(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tickCtx, cancel := trace.NewBackground(ctx, "outbox-cleanup", w.logger, cleanupInterval)
			affected, err := w.outboxRepo.DeleteOldDone(tickCtx, 7*24*time.Hour)
			cancel()
			if err != nil {
				trace.Logger(tickCtx, w.logger).Error("outbox cleanup: delete old done records failed", zap.Error(err))
				continue
			}
			if affected > 0 {
				trace.Logger(tickCtx, w.logger).Info("outbox cleanup: deleted old MYSQL_DONE records",
					zap.Int64("count", affected))
			}
		}
	}
}

// computePendingOutboxDeltas 计算未完成 outbox 记录中各账户的净余额变动
// 供 warmAccounts 使用，确保预热到 Redis 的余额包含已执行但尚未落 MySQL 的交易。
func computePendingOutboxDeltas(records []*model.SettlementOutbox) map[string]int64 {
	deltas := make(map[string]int64)
	for _, rec := range records {
		var event model.SettlementEvent
		if err := json.Unmarshal([]byte(rec.EventData), &event); err != nil {
			continue
		}
		for _, entry := range event.Entries {
			d, _ := strconv.ParseInt(entry.BalanceDelta, 10, 64)
			deltas[entry.AccountNo] += d
		}
	}
	return deltas
}

package service

// FreezeCompensateOutboxWorker 兜底 UnfreezeAndDebit 失败时未被 caller 自己
// inline 补偿成功的场景：
//
//   场景: caller 写完 Phase 1（frozen-debit + outbox PENDING）→ 进 Phase 2 →
//         任一 entry 失败 → caller 尝试 inline 补偿 → 补偿本身也失败（DB 抖、
//         进程崩在补偿中途）→ outbox 留 PENDING（caller 已经返回 error 给业务）
//   兜底: 本 worker 60s 后扫到 PENDING，CAS Claim → 调
//         freezeService.compensateCreditEntry / compensateFrozenDebit →
//         MarkCompensated；失败重试到 maxRetry → MarkFailed + CRITICAL 告警
//
// 多 pod 下用 etcd leader election 限只一个 pod 跑，避免并发对同一 voucher
// 跑两次 compensate（虽然 ClaimPending CAS 也能挡，但少 DB 锁竞争）。

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/repository"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
)

const (
	// 给 caller 60s 时间跑 Phase 2 + 标记 DONE，超过这个阈值 worker 才介入。
	freezeCompensatePendingThreshold = 60 * time.Second
	freezeCompensateMaxRetry         = 5
	freezeCompensateScanInterval     = 30 * time.Second
	freezeCompensateBatchSize        = 50
)

var (
	freezeCompensateRecoveredTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_freeze_compensate_recovered_total",
		Help: "Number of UnfreezeAndDebit operations recovered by the compensate outbox worker.",
	})
	freezeCompensateFailedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_freeze_compensate_failed_total",
		Help: "Number of UnfreezeAndDebit compensate attempts that exhausted retries (manual intervention required).",
	})
	freezeCompensateScanRuns = promauto.NewCounter(prometheus.CounterOpts{
		Name: "accounting_freeze_compensate_scan_runs_total",
		Help: "Number of compensate outbox scan iterations.",
	})
)

// FreezeCompensateOutboxWorker 兜底 worker
type FreezeCompensateOutboxWorker struct {
	freezeSvc      *freezeService
	outbox         repository.FreezeCompensateOutboxRepository
	logger         *zap.Logger
	wg             sync.WaitGroup
	stoppedCounter atomic.Int64
}

// NewFreezeCompensateOutboxWorker 构造。
//
// 依赖 *freezeService 的具体类型而不是接口，因为我们要直接调它的 (private)
// compensateCreditEntry / compensateFrozenDebit 方法。同 package 内合法。
func NewFreezeCompensateOutboxWorker(
	svc FreezeService,
	outbox repository.FreezeCompensateOutboxRepository,
	logger *zap.Logger,
) *FreezeCompensateOutboxWorker {
	concrete, _ := svc.(*freezeService)
	return &FreezeCompensateOutboxWorker{
		freezeSvc: concrete,
		outbox:    outbox,
		logger:    logger.Named("freeze-compensate-worker"),
	}
}

// Start 启动后台扫描循环
func (w *FreezeCompensateOutboxWorker) Start(ctx context.Context) {
	if w.outbox == nil {
		w.logger.Warn("freeze compensate outbox not wired; worker disabled (dev/legacy)")
		return
	}
	if w.freezeSvc == nil {
		w.logger.Warn("freeze service concrete type unavailable; worker disabled")
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(freezeCompensateScanInterval)
		defer ticker.Stop()
		w.logger.Info("freeze compensate outbox worker started",
			zap.Duration("scan_interval", freezeCompensateScanInterval),
			zap.Duration("pending_threshold", freezeCompensatePendingThreshold))
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 每 tick 起 background ctx：trace_id 新发 + shadow=false。
				// 兜底补偿写主账，绝不能漏带 shadow 标。
				tickCtx, cancel := trace.NewBackground(ctx, "freeze-compensate", w.logger, freezeCompensateScanInterval)
				w.scanOnce(tickCtx)
				cancel()
			}
		}
	}()
}

// Wait 阻塞等 worker goroutine 退出
func (w *FreezeCompensateOutboxWorker) Wait() { w.wg.Wait() }

// scanOnce 扫一轮
func (w *FreezeCompensateOutboxWorker) scanOnce(ctx context.Context) {
	freezeCompensateScanRuns.Inc()
	rows, err := w.outbox.ListPendingExpired(ctx, freezeCompensatePendingThreshold, freezeCompensateBatchSize)
	if err != nil {
		w.logger.Error("freeze compensate scan failed", zap.Error(err))
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		w.processOne(ctx, row)
	}
}

// processOne 处理单条 outbox 记录
func (w *FreezeCompensateOutboxWorker) processOne(ctx context.Context, row *model.FreezeCompensateOutbox) {
	// 已达 max retry → 直接 MarkFailed 不再尝试
	if row.RetryCount >= freezeCompensateMaxRetry {
		_ = w.markFailedAfterRetries(ctx, row)
		return
	}

	// CAS PENDING → COMPENSATING；0 rows = 别人抢了或状态变了，跳过
	claimed, err := w.outbox.ClaimPending(ctx, row.VoucherNo)
	if err != nil {
		w.logger.Error("freeze compensate claim failed", zap.String("voucher", row.VoucherNo), zap.Error(err))
		return
	}
	if !claimed {
		return
	}

	if err := w.runCompensate(ctx, row); err != nil {
		w.logger.Error("freeze compensate execution failed",
			zap.String("voucher", row.VoucherNo),
			zap.Int("retry", row.RetryCount+1),
			zap.Error(err))
		// 失败 → IncrementRetry 把状态从 COMPENSATING 推回 PENDING（下轮 retry）。
		// 注意：retry_count 在 IncrementRetry 内部 +1。
		if iErr := w.outbox.IncrementRetry(ctx, row.VoucherNo, err.Error()); iErr != nil {
			w.logger.Error("IncrementRetry failed", zap.String("voucher", row.VoucherNo), zap.Error(iErr))
		}
		return
	}

	// 成功 → COMPENSATING → DONE
	if err := w.outbox.MarkCompensated(ctx, row.VoucherNo); err != nil {
		w.logger.Error("MarkCompensated failed", zap.String("voucher", row.VoucherNo), zap.Error(err))
		return
	}
	freezeCompensateRecoveredTotal.Inc()
	w.logger.Info("freeze compensate succeeded",
		zap.String("voucher", row.VoucherNo),
		zap.String("freezeOrderNo", row.FreezeOrderNo))
}

func (w *FreezeCompensateOutboxWorker) markFailedAfterRetries(ctx context.Context, row *model.FreezeCompensateOutbox) error {
	// 先把行抢到 COMPENSATING 才能 MarkFailed；如果状态已变（被其他 worker 处理）就放弃
	if claimed, err := w.outbox.ClaimPending(ctx, row.VoucherNo); err != nil || !claimed {
		return err
	}
	freezeCompensateFailedTotal.Inc()
	w.logger.Error("CRITICAL: freeze compensate exhausted retries; manual intervention required",
		zap.String("voucher", row.VoucherNo),
		zap.String("freezeOrderNo", row.FreezeOrderNo),
		zap.String("last_error", row.ErrorMsg),
		zap.Int("retry_count", row.RetryCount))
	return w.outbox.MarkFailed(ctx, row.VoucherNo, row.ErrorMsg)
}

// runCompensate 执行实际反向 SQL：依次反向所有 credit entries + 反向 frozen-debit。
//
// 幂等性：每条 credit 反向用 reverseTxID = origTxID + "-R"，第二次重跑会
// ON DUPLICATE KEY 跳过（applyCreditEntry / compensateCreditEntry 内部）。
// frozen-debit 反向生成新 txID，但 frozen account 的 balance += A 累计幂等
// 性靠 outbox 状态机（CAS PENDING→COMPENSATING）保证只跑一次，所以可以安全
// 加上 frozen_balance += A 而不重复加。
func (w *FreezeCompensateOutboxWorker) runCompensate(ctx context.Context, row *model.FreezeCompensateOutbox) error {
	payload, err := model.UnmarshalCompensatePayload(row.Payload)
	if err != nil {
		return err
	}

	now := time.Unix(payload.NowUnix, 0)
	req := &UnfreezeAndDebitRequest{
		FreezeOrderNo:      row.FreezeOrderNo,
		FreezeAccountNo:    payload.FreezeAccountNo,
		FreezeBusinessNo:   payload.FreezeBusinessNo,
		FreezeBusinessType: model.BusinessType(payload.FreezeBusinessType),
		Currency:           payload.Currency,
		Description:        payload.Description,
	}

	// 反向所有 credit entries（idempotent via reverseTxID 唯一索引）。
	//
	// 关键正确性：先查原 transaction_id 是否真的存在；不存在说明那条 credit
	// Phase 2 时根本没落库（caller inline 补偿失败前还没走到那条），反向就是
	// 凭空 debit，会让账户余额莫名其妙减少 → 资损。所以**只反向已落库的**。
	for _, e := range payload.CreditEntries {
		existing, gErr := w.freezeSvc.transactionRepo.GetTransactionByID(ctx, e.TxID)
		if gErr != nil {
			return fmt.Errorf("lookup original credit tx %s: %w", e.TxID, gErr)
		}
		if existing == nil {
			// 原 credit 没落库 → 跳过（worker compensate 不需要反向不存在的 entry）
			w.logger.Info("freeze compensate: skip reversing non-existent credit (Phase 2 never reached this entry)",
				zap.String("voucher", row.VoucherNo),
				zap.String("origTxID", e.TxID),
				zap.String("accountNo", e.AccountNo))
			continue
		}
		entry := UnfreezeEntry{
			AccountNo: e.AccountNo, DebitAmount: e.DebitAmount,
			CreditAmount: e.CreditAmount, Description: e.Description,
		}
		if err := w.freezeSvc.compensateCreditEntry(ctx, entry, e.TxID, row.VoucherNo,
			req, payload.TransactionDate, payload.CutDate, now); err != nil {
			return err
		}
	}

	// 反向 frozen-debit
	dbIdx, tableIdx := w.freezeSvc.router.RouteByAccountNo(payload.FreezeAccountNo)
	if err := w.freezeSvc.compensateFrozenDebit(ctx, req, payload.FreezeAccountNo, payload.FreezeAmount,
		dbIdx, tableIdx, row.VoucherNo, payload.TransactionDate, payload.CutDate, now); err != nil {
		return err
	}
	return nil
}

package service

// TccRecoveryWorker TCC 悬挂事务自动回滚工作器
//
// 职责：
//   每 30s 双轨扫描：
//   1. 扫描 tcc_coordinator 表（meta DB）：
//      - TRYING 且超时  → CancelTcc（释放冻结资金）
//      - CONFIRMING 且超时 → CRITICAL 告警，不取消（防止已提交的 Confirm 被错误补偿）
//   2. 扫描 tcc_transaction 分支表（兼容无 coordinator 的旧记录）：
//      - TRYING 且不在 CONFIRMING 协调者中 → CancelTcc
//
// 金融安全（Risk A 修复）：
//   Confirm 阶段开始前 coordinator 写入 CONFIRMING；RecoveryWorker 看到 CONFIRMING
//   时只告警、不取消，防止部分 Confirm 后资金被错误抵消导致账务不平。
//   CONFIRMING 悬挂需人工或专用工具 RetryConfirmTcc 处理。
//
// 幂等性：
//   CancelBranch 检查 branch.Status；已 CANCELLED 的分支直接跳过，重复运行安全。

import (
	"context"
	"sync"
	"time"

	"github.com/accounting-system/internal/domain/model"
	"github.com/accounting-system/internal/metrics"
	"github.com/accounting-system/internal/repository"
	"go.uber.org/zap"
)

const (
	tccRecoveryInterval     = 30 * time.Second // 扫描间隔
	tccStuckTimeoutMinutes  = 5                // TRYING 超过此分钟数视为悬挂
	tccRecoveryBatchLimit   = 100              // 每次最多取回的分支数
	tccRecoveryQueryTimeout = 10 * time.Second
)

// ConfirmRetrier CONFIRMING 半挂起的恢复接口（accountingService 实现）。
// 如果 nil，TccRecoveryWorker 退化为只告警不修复（旧行为）。
type ConfirmRetrier interface {
	RetryStuckConfirmingTcc(ctx context.Context, before time.Time) (int, error)
}

// TccRecoveryWorker TCC 悬挂事务自动回滚工作器
type TccRecoveryWorker struct {
	tccSvc    TccService
	coordRepo repository.TccCoordinatorRepository // 可为 nil（旧部署兼容）
	retrier   ConfirmRetrier                      // 可为 nil（仅告警不修 CONFIRMING）
	logger    *zap.Logger
	wg        sync.WaitGroup
}

// NewTccRecoveryWorker 创建 TccRecoveryWorker
func NewTccRecoveryWorker(tccSvc TccService, coordRepo repository.TccCoordinatorRepository, retrier ConfirmRetrier, logger *zap.Logger) *TccRecoveryWorker {
	return &TccRecoveryWorker{
		tccSvc:    tccSvc,
		coordRepo: coordRepo,
		retrier:   retrier,
		logger:    logger,
	}
}

// Start 启动后台 goroutine（随 ctx 取消而退出）
func (w *TccRecoveryWorker) Start(ctx context.Context) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx)
	}()
}

// Wait 阻塞等待后台 goroutine 退出。
func (w *TccRecoveryWorker) Wait() {
	w.wg.Wait()
}

func (w *TccRecoveryWorker) run(ctx context.Context) {
	ticker := time.NewTicker(tccRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.recover(ctx)
		}
	}
}

// recover 三轨扫描：
//  1. 协调者表 TRYING 超时 → CancelTcc
//  2. 协调者表 CONFIRMING 超时 → RetryConfirm（如果配置了 retrier；否则仅告警）
//  3. 分支表兜底（兼容旧记录）
func (w *TccRecoveryWorker) recover(ctx context.Context) {
	// 轨道 1：扫描 tcc_coordinator 表（首选路径）
	confirmingIDs := w.recoverFromCoordinator(ctx)

	// 轨道 2：CONFIRMING 半挂起的批量重试（修复"60K 试算不平"那类问题）
	if w.retrier != nil && len(confirmingIDs) > 0 {
		before := time.Now().Add(-time.Duration(tccStuckTimeoutMinutes) * time.Minute)
		if n, err := w.retrier.RetryStuckConfirmingTcc(ctx, before); err != nil {
			w.logger.Error("tcc recovery: retry CONFIRMING failed", zap.Error(err))
		} else if n > 0 {
			w.logger.Warn("tcc recovery: recovered CONFIRMING half-confirmed TCCs",
				zap.Int("recovered", n),
				zap.Int("scanned", len(confirmingIDs)))
		}
	}

	// 轨道 3：扫描分支表（兼容旧记录或 coordinator 写入失败场景）
	// 跳过已在 CONFIRMING coordinator 中的 TCC，防止错误取消正在 Confirm 的事务
	w.recoverFromBranches(ctx, confirmingIDs)
}

// recoverFromCoordinator 扫描 tcc_coordinator 表：
//   - TRYING 超时 → CancelTcc
//   - CONFIRMING 超时 → CRITICAL 告警，不取消
//
// 返回当前处于 CONFIRMING 阶段的 tccID 集合，供分支扫描排除。
func (w *TccRecoveryWorker) recoverFromCoordinator(ctx context.Context) map[string]struct{} {
	confirmingIDs := make(map[string]struct{})
	if w.coordRepo == nil {
		return confirmingIDs
	}

	before := time.Now().Add(-time.Duration(tccStuckTimeoutMinutes) * time.Minute)
	qCtx, cancel := context.WithTimeout(ctx, tccRecoveryQueryTimeout)
	coords, err := w.coordRepo.ListStuck(qCtx, before, tccRecoveryBatchLimit)
	cancel()

	if err != nil {
		w.logger.Error("tcc recovery: list stuck coordinators failed", zap.Error(err))
		return confirmingIDs
	}

	for _, coord := range coords {
		switch coord.Phase {
		case model.TccPhaseConfirming:
			// CONFIRMING 超时：不能取消！可能部分 Confirm 已提交，取消会导致余额不平。
			// 仅告警，等待人工介入（或 RetryConfirmTcc 工具重试）。
			confirmingIDs[coord.TccID] = struct{}{}
			metrics.TccRecoveryFailuresTotal.Inc()
			w.logger.Error("CRITICAL: TCC stuck in CONFIRMING phase — partial confirm may have occurred, manual intervention required. DO NOT auto-cancel.",
				zap.String("tccID", coord.TccID),
				zap.String("businessNo", coord.BusinessNo),
				zap.Time("updatedAt", coord.UpdatedAt),
			)

		case model.TccPhaseTrying:
			// TRYING 超时：正常取消路径（Try 未完成或全部失败）
			w.logger.Warn("tcc recovery (coordinator): cancelling stuck TRYING TCC",
				zap.String("tccID", coord.TccID),
				zap.String("businessNo", coord.BusinessNo),
			)
			if cancelErr := w.tccSvc.CancelTcc(ctx, coord.TccID); cancelErr != nil {
				metrics.TccRecoveryFailuresTotal.Inc()
				w.logger.Error("CRITICAL: tcc recovery failed — frozen balance NOT released, manual intervention required",
					zap.String("tccID", coord.TccID),
					zap.Error(cancelErr),
				)
			} else {
				metrics.TccRecoveredTotal.Inc()
				if cErr := w.coordRepo.TransitionToCancelled(ctx, coord.TccID); cErr != nil {
					w.logger.Warn("tcc coordinator cancel transition failed after cancel",
						zap.String("tccID", coord.TccID), zap.Error(cErr))
				}
				w.logger.Info("tcc recovery (coordinator): TCC cancelled and frozen balance released",
					zap.String("tccID", coord.TccID),
				)
			}
		}
	}
	return confirmingIDs
}

// recoverFromBranches 扫描悬挂 TRYING 分支，按 tccID 去重后逐个 CancelTcc。
// skipIDs 为已知处于 CONFIRMING 阶段的 tccID，跳过这些以防错误取消。
//
// 为什么按 tccID 分组而非逐条 CancelBranch：
//   TCC 是全有全无协议；若一个 TCC 下有任意分支超时，整个事务应全部回滚。
//   CancelTcc 会重新扫描该 tccID 的全量分支，确保 CONFIRMED 状态检测正确。
func (w *TccRecoveryWorker) recoverFromBranches(ctx context.Context, skipIDs map[string]struct{}) {
	qCtx, cancel := context.WithTimeout(ctx, tccRecoveryQueryTimeout)
	stuckBranches, err := w.tccSvc.ListStuck(qCtx, tccStuckTimeoutMinutes, tccRecoveryBatchLimit)
	cancel()

	if err != nil {
		w.logger.Error("tcc recovery: list stuck branches failed", zap.Error(err))
		return
	}
	if len(stuckBranches) == 0 {
		return
	}

	// 按 tccID 去重，对每个 TCC 事务只执行一次 CancelTcc
	seen := make(map[string]struct{}, len(stuckBranches))
	for _, b := range stuckBranches {
		tccID := b.TccID
		if _, ok := seen[tccID]; ok {
			continue
		}
		seen[tccID] = struct{}{}

		// 跳过 CONFIRMING 阶段的 TCC（已由 coordinator 扫描处理并告警）
		if _, skip := skipIDs[tccID]; skip {
			continue
		}

		w.logger.Warn("tcc recovery (branch scan): cancelling stuck TCC",
			zap.String("tccID", tccID),
			zap.String("branchID", b.BranchID),
			zap.String("accountNo", b.AccountNo),
			zap.Int("stuckMinutes", tccStuckTimeoutMinutes),
		)

		if cancelErr := w.tccSvc.CancelTcc(ctx, tccID); cancelErr != nil {
			// 冻结资金未成功释放 — 打 CRITICAL 告警，等待人工介入
			metrics.TccRecoveryFailuresTotal.Inc()
			w.logger.Error("CRITICAL: tcc recovery failed — frozen balance NOT released, manual intervention required",
				zap.String("tccID", tccID),
				zap.Error(cancelErr),
			)
		} else {
			metrics.TccRecoveredTotal.Inc()
			w.logger.Info("tcc recovery (branch scan): TCC cancelled and frozen balance released",
				zap.String("tccID", tccID),
			)
		}
	}
}

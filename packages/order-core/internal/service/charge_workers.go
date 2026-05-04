package service

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/repo"
)

// ─── ChargeExpireWorker：过期 Charge 终态化 ─────────────────────────────────

// ChargeExpireWorker 扫描 expired_at<now 且仍 pending 的 Charge → expired
// 同时把 PI 如果在 processing/requires_action → failed
type ChargeExpireWorker struct {
	chargeRepo repo.ChargeRepository
	piSvc      PaymentIntentService
	// reconcile + registry + chName 用于 expireOne 之前先 Query channel：
	// 如果 channel 实际已成功（webhook 漏发）就走 OnLateChargeSuccess 兜底，
	// 而不是直接 MarkFailed 把客户钱坑在 channel。
	reconcile *RefundReconcileService
	registry  channel.PaymentChannelRegistry
	chName    string
	interval  time.Duration
	limit     int
	logger    *zap.Logger
}

// NewChargeExpireWorker 构造
func NewChargeExpireWorker(cr repo.ChargeRepository, ps PaymentIntentService,
	reconcile *RefundReconcileService, registry channel.PaymentChannelRegistry, chName string,
	interval time.Duration, limit int, logger *zap.Logger) *ChargeExpireWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	if limit <= 0 {
		limit = 200
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ChargeExpireWorker{
		chargeRepo: cr, piSvc: ps,
		reconcile: reconcile, registry: registry, chName: chName,
		interval: interval, limit: limit, logger: logger,
	}
}

// Start 阻塞运行
func (w *ChargeExpireWorker) Start(ctx context.Context) {
	w.logger.Info("charge expire worker started", zap.Duration("interval", w.interval))
	t := time.NewTicker(w.interval)
	defer t.Stop()
	run := func() {
		list, err := w.chargeRepo.ListExpired(ctx, time.Now().UTC(), w.limit)
		if err != nil {
			w.logger.Warn("list expired charges failed", zap.Error(err))
			return
		}
		for _, c := range list {
			w.expireOne(ctx, c)
		}
		if len(list) > 0 {
			w.logger.Info("expired charges swept", zap.Int("count", len(list)))
		}
	}
	run()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("charge expire worker stopped")
			return
		case <-t.C:
			run()
		}
	}
}

func (w *ChargeExpireWorker) expireOne(ctx context.Context, c *domain.Charge) {
	// 资金安全：在标 EXPIRED 之前先 Query channel —— 如果 channel 实际已成功
	// （webhook 漏发），直接 MarkFailed 会把客户钱坑死（channel 那边收了钱
	// 但本地标失败 → accounting 没入账 → merchant 没钱 + 客户找不到记录）。
	// 同步 Query：channel succeeded → 走 OnLateChargeSuccess 兜底 enqueue
	// accounting outbox；processing → 不标 EXPIRED 等下轮；failed / Query 失败 → MarkFailed。
	if w.registry != nil && w.reconcile != nil && w.chName != "" {
		if pc := w.registry.Get(w.chName); pc != nil {
			resp, qErr := pc.Query(ctx, channel.QueryRequest{
				PaymentIntentID: c.PaymentIntentID,
				ChargeID:        c.ID,
				ExternalRefNo:   c.BalanceTransaction,
			})
			if qErr == nil && resp != nil {
				switch resp.ResultType {
				case channel.PaymentResultSucceeded:
					w.logger.Warn("expire worker: channel says SUCCEEDED, routing to late-success (webhook likely lost)",
						zap.String("pi_id", c.PaymentIntentID),
						zap.String("charge_id", c.ID),
						zap.Int64("amount_captured", resp.AmountCaptured))
					_ = w.reconcile.OnLateChargeSuccess(ctx, c.PaymentIntentID, c.ID, resp.AmountCaptured)
					return
				case channel.PaymentResultProcessing:
					w.logger.Info("expire worker: channel still processing, skip expiring this round",
						zap.String("charge_id", c.ID))
					return
				}
				// PaymentResultFailed → fall through 走 EXPIRED 路径
			}
		}
	}

	now := time.Now().UTC()
	// 资损修复：CAS pending → expired，避免 ExpireWorker 跟 webhook (charge.succeeded)
	// race 时把已经 succeeded 的 charge 错误覆盖回 expired。CAS won=false 时安静
	// skip，让 succeeded 那一侧的副作用（accounting outbox enqueue 等）保留。
	_, won, err := w.chargeRepo.CASUpdateStatus(ctx, c.PaymentIntentID, c.ID,
		domain.ChargeStatusPending, domain.ChargeStatusExpired, map[string]any{
			"failure_code":    "charge_expired",
			"failure_message": "charge expired before terminal",
			"completed_at":    now,
		})
	if err != nil {
		w.logger.Warn("mark charge expired failed", zap.String("charge_id", c.ID), zap.Error(err))
		return
	}
	if !won {
		w.logger.Info("expire worker: CAS skipped (charge already terminal)",
			zap.String("charge_id", c.ID))
		return
	}
	// 驱动 PI → failed
	_, _ = w.piSvc.MarkFailed(ctx, c.PaymentIntentID, c.ID, "charge_expired", "auto expired")
}

// ─── ReconcileWorker：跨分片扫 Charge → channel.Query → 矫正本地状态 ──────

// ReconcileWorker 对账 worker：扫长时间 pending 的 Charge，调 channel.Query 拉取真实状态。
// 适用于渠道 webhook 丢失 / 晚到的场景。
type ReconcileWorker struct {
	chargeRepo repo.ChargeRepository
	piSvc      PaymentIntentService
	reconcile  *RefundReconcileService
	registry   channel.PaymentChannelRegistry
	chName     string
	interval   time.Duration
	staleAfter time.Duration // 多久算 stale：pending 超过这个时长才 Query
	limit      int
	logger     *zap.Logger
}

// NewReconcileWorker 构造
func NewReconcileWorker(
	cr repo.ChargeRepository,
	ps PaymentIntentService,
	reconcile *RefundReconcileService,
	reg channel.PaymentChannelRegistry,
	chName string,
	interval, staleAfter time.Duration,
	limit int,
	logger *zap.Logger,
) *ReconcileWorker {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if staleAfter <= 0 {
		staleAfter = 10 * time.Minute
	}
	if limit <= 0 {
		limit = 200
	}
	if chName == "" {
		chName = "payment-core"
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ReconcileWorker{
		chargeRepo: cr, piSvc: ps, reconcile: reconcile, registry: reg, chName: chName,
		interval: interval, staleAfter: staleAfter, limit: limit, logger: logger,
	}
}

// Start 阻塞运行
func (w *ReconcileWorker) Start(ctx context.Context) {
	w.logger.Info("reconcile worker started",
		zap.Duration("interval", w.interval),
		zap.Duration("stale_after", w.staleAfter))
	t := time.NewTicker(w.interval)
	defer t.Stop()
	run := func() {
		cutoff := time.Now().UTC().Add(-w.staleAfter)
		list, err := w.chargeRepo.ListPendingForReconcile(ctx, cutoff, w.limit)
		if err != nil {
			w.logger.Warn("list pending charges for reconcile failed", zap.Error(err))
			return
		}
		if len(list) == 0 {
			return
		}
		pc := w.registry.Get(w.chName)
		if pc == nil {
			w.logger.Warn("no channel registered for reconcile", zap.String("name", w.chName))
			return
		}
		for _, c := range list {
			w.reconcileOne(ctx, pc, c)
		}
		w.logger.Info("reconcile pass completed", zap.Int("count", len(list)))
	}
	run()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("reconcile worker stopped")
			return
		case <-t.C:
			run()
		}
	}
}

func (w *ReconcileWorker) reconcileOne(ctx context.Context, pc channel.PaymentChannel, c *domain.Charge) {
	resp, err := pc.Query(ctx, channel.QueryRequest{
		PaymentIntentID: c.PaymentIntentID,
		ChargeID:        c.ID,
		ExternalRefNo:   c.BalanceTransaction,
	})
	if err != nil {
		w.logger.Debug("reconcile query failed", zap.String("charge_id", c.ID), zap.Error(err))
		return
	}
	switch resp.ResultType {
	case channel.PaymentResultSucceeded:
		// 渠道告知已成功：本地若仍 pending，按"晚到成功"路径推进
		if w.reconcile != nil {
			_ = w.reconcile.OnLateChargeSuccess(ctx, c.PaymentIntentID, c.ID, resp.AmountCaptured)
		} else {
			_, _ = w.piSvc.MarkSucceeded(ctx, c.PaymentIntentID, c.ID, resp.AmountCaptured)
		}
	case channel.PaymentResultFailed:
		_, _ = w.piSvc.MarkFailed(ctx, c.PaymentIntentID, c.ID, resp.FailureCode, resp.FailureMessage)
	case channel.PaymentResultProcessing:
		// 渠道仍在处理，不做动作，等下次
	}
}

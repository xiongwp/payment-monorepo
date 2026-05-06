package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
	"github.com/xiongwp/order-core/internal/sharding"
	"github.com/xiongwp/payment-util/trace"
)

// RefundReconcileService 处理晚到的渠道成功回调与退款的最终一致性。
//
// 场景：
//   1. 商户之前发起过退款，因渠道不响应被 cron 标记 expired → PI.RefundPhase = refund_failed
//   2. 之后渠道真的把钱扣走了（晚到的 charge success webhook）
//   3. 系统必须：
//        a) 把相关 Charge 置为 succeeded（若仍 pending）
//        b) 把 PI 从 processing / pending 推进到 succeeded
//        c) 针对之前已 failed 的退款，自动补一笔新退款单（auto_compensate=1）
//        d) RefundRetryWorker 无限重试直到成功
type RefundReconcileService struct {
	piSvc      PaymentIntentService
	refundSvc  RefundService
	piRepo     repo.PaymentIntentRepository
	chargeRepo repo.ChargeRepository
	refundRepo repo.RefundRepository
	// accounting 用于 OnLateChargeSuccess 时 enqueue accounting outbox（防止
	// webhook 漏发导致 merchant 账上没钱）。可选 nil（dev / 测试不接 accounting 时）。
	accounting AccountingOutboxService
	idgen      idgen.IDGenerator
	router     *sharding.Router
	logger     *zap.Logger
}

// NewRefundReconcileService 构造
func NewRefundReconcileService(
	piSvc PaymentIntentService,
	refundSvc RefundService,
	piRepo repo.PaymentIntentRepository,
	chargeRepo repo.ChargeRepository,
	refundRepo repo.RefundRepository,
	accounting AccountingOutboxService,
	g idgen.IDGenerator,
	r *sharding.Router,
	logger *zap.Logger,
) *RefundReconcileService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RefundReconcileService{
		piSvc: piSvc, refundSvc: refundSvc, piRepo: piRepo,
		chargeRepo: chargeRepo, refundRepo: refundRepo,
		accounting: accounting,
		idgen:      g, router: r, logger: logger,
	}
}

// OnLateChargeSuccess 晚到的渠道成功回调：先把 PI / Charge 推到 succeeded，
// 再检查是否有已 failed 的退款单，如有就按原金额自动补退。
func (s *RefundReconcileService) OnLateChargeSuccess(ctx context.Context, piID, chargeID string, amountCaptured int64) error {
	// 1) 先把 Charge 置为 succeeded
	_, _ = s.chargeRepo.UpdateFields(ctx, piID, chargeID, map[string]any{
		"status":          domain.ChargeStatusSucceeded,
		"amount_captured": amountCaptured,
		"captured":        true,
		"paid":            true,
		"completed_at":    time.Now().UTC(),
	})

	// 2) 推进 PI → succeeded（内部校验 CanTransition，已终态时 no-op）
	pi, err := s.piSvc.MarkSucceeded(ctx, piID, chargeID, amountCaptured)
	if err != nil {
		s.logger.Warn("late success: mark pi succeeded failed", zap.String("pi_id", piID), zap.Error(err))
		// 即便 MarkSucceeded 失败（如 PI 已终态），后面也尝试 enqueue accounting，
		// 因为 RequestID 派生自 (pi_id, charge_id) 是 deterministic，accounting_outbox
		// 的 UNIQUE KEY 会防重复入队。
	}

	// 2.5) **资金安全关键**：webhook 漏发时由本路径兜底 enqueue accounting outbox。
	// 之前漏了这步 → ReconcileWorker / OnLateChargeSuccess 把 PI / Charge 标
	// SUCCEEDED 但 accounting 永远没收到 → merchant 账上永远没钱。
	// EnqueueChargeSucceeded 用 deterministic RequestID = "{pi_id}:charge_succeeded:{charge_id}"
	// + DB UNIQUE 兜底；正常 webhook 路径已经 enqueue 过的话，本次 INSERT 会
	// DUPLICATE KEY → 幂等无害。
	if s.accounting != nil && pi != nil {
		if enqErr := s.accounting.EnqueueChargeSucceeded(ctx, pi, chargeID, amountCaptured); enqErr != nil {
			s.logger.Error("late success: accounting outbox enqueue failed (will rely on retry)",
				zap.String("pi_id", piID), zap.String("charge_id", chargeID), zap.Error(enqErr))
		}
	}

	// 3) 扫本 PI 下所有 failed 的退款，按金额合计自动补退
	refunds, err := s.refundRepo.ListByPI(ctx, piID)
	if err != nil {
		return err
	}
	// **资金安全幂等**：OnLateChargeSuccess 可能被 webhook + reconcile + retry
	// 多次触发，每次都计算 FAILED refund 的总额并补退，会重复创建补偿退款 →
	// 客户拿到双倍退款。
	// 用 metadata["reconcile"]="late_charge_success" 标记本 PI 是否已经发起过
	// 自动补偿；如已存在（不论 PENDING/SUCCEEDED）→ 跳过本次。
	for _, rf := range refunds {
		if rf.Metadata != nil && rf.Metadata["reconcile"] == "late_charge_success" {
			s.logger.Info("late success: auto-refund already created previously, skip duplicate compensation",
				zap.String("pi_id", piID),
				zap.String("existing_refund_id", rf.ID),
				zap.String("existing_status", string(rf.Status)))
			return nil
		}
	}
	var compensateAmount int64
	for _, rf := range refunds {
		if rf.Status == domain.RefundStatusFailed {
			compensateAmount += rf.Amount
		}
	}
	if compensateAmount <= 0 {
		return nil
	}
	s.logger.Info("late success reconcile: auto-refund",
		zap.String("pi_id", piID), zap.Int64("amount", compensateAmount))
	bundle, err := s.refundSvc.CreateBundle(ctx, &CreateRefundInput{
		PaymentIntentID: piID,
		Amount:          compensateAmount,
		Reason:          domain.RefundReasonRequestedByCustomer,
		Metadata:        map[string]string{"reconcile": "late_charge_success"},
	})
	if err != nil {
		return err
	}
	// 全部标 auto_compensate=1；retry worker 会无限重试
	for _, rf := range bundle.Refunds {
		_, _ = s.refundRepo.UpdateFields(ctx, piID, rf.ID, map[string]any{
			"auto_compensate": true,
			"next_retry_at":   time.Now().UTC(),
		})
	}
	return nil
}

// ─── RefundRetryWorker：失败 / pending 退款重试直到成功 ────────────────────────

// RefundRetryWorker 周期性扫分片，找出需要重试的退款单，调渠道推进。
//
// 处理规则：
//   - status=pending && next_retry_at<=now      → 走渠道（首次或未得到结果）
//   - status=failed && auto_compensate=1         → 无限重试
//   - status=failed && auto_compensate=0         → 不重试（由用户自行新建）
type RefundRetryWorker struct {
	refundRepo repo.RefundRepository
	refundSvc  RefundService
	registry   channel.PaymentChannelRegistry
	chName     string
	interval   time.Duration
	limit      int
	logger     *zap.Logger
}

// NewRefundRetryWorker 构造
func NewRefundRetryWorker(
	rr repo.RefundRepository,
	rs RefundService,
	reg channel.PaymentChannelRegistry,
	chName string,
	interval time.Duration,
	limit int,
	logger *zap.Logger,
) *RefundRetryWorker {
	if interval <= 0 {
		interval = time.Minute
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
	return &RefundRetryWorker{
		refundRepo: rr, refundSvc: rs, registry: reg, chName: chName,
		interval: interval, limit: limit, logger: logger,
	}
}

// Start 阻塞运行
func (w *RefundRetryWorker) Start(ctx context.Context) {
	w.logger.Info("refund retry worker started", zap.Duration("interval", w.interval))
	t := time.NewTicker(w.interval)
	defer t.Stop()
	runOnce := func() {
		// trace.NewBackground 每 tick 一次：trace_id 新生成 + shadow=false
		// 强制。refund 路径要写主表 + 调真渠道，不能 shadow 漂移。
		bgCtx, cancel := trace.NewBackground(ctx, "refund-retry-worker", w.logger, w.interval)
		defer cancel()
		if err := w.sweep(bgCtx); err != nil {
			trace.Logger(bgCtx, w.logger).Warn("refund retry sweep error", zap.Error(err))
		}
	}
	runOnce()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("refund retry worker stopped")
			return
		case <-t.C:
			runOnce()
		}
	}
}

// sweep 扫描全部分片，针对每笔到期的 refund 调渠道
func (w *RefundRetryWorker) sweep(ctx context.Context) error {
	list, err := w.refundRepo.ListRetryDue(ctx, time.Now().UTC(), w.limit)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	pc := w.registry.Get(w.chName)
	if pc == nil {
		return fmt.Errorf("payment channel %q not registered", w.chName)
	}
	for _, rf := range list {
		w.tryOne(ctx, pc, rf)
	}
	return nil
}

// tryOne 推一笔退款
func (w *RefundRetryWorker) tryOne(ctx context.Context, pc channel.PaymentChannel, rf *domain.Refund) {
	resp, err := pc.Refund(ctx, channel.RefundChannelRequest{
		PaymentIntentID: rf.PaymentIntentID,
		ChargeID:        rf.ChargeID,
		RefundID:        rf.ID,
		Amount:          rf.Amount,
		Currency:        rf.Currency,
		Reason:          string(rf.Reason),
	})
	if err != nil {
		w.scheduleNext(ctx, rf, "channel error: "+err.Error())
		return
	}
	switch resp.ResultType {
	case channel.PaymentResultSucceeded:
		if _, err := w.refundSvc.MarkSucceeded(ctx, rf.PaymentIntentID, rf.ID); err != nil {
			w.logger.Warn("mark refund succeeded failed", zap.Error(err), zap.String("refund_id", rf.ID))
		}
	case channel.PaymentResultProcessing:
		// 渠道还在处理，保持 pending，推迟下次重试
		w.scheduleNext(ctx, rf, "channel processing")
	case channel.PaymentResultFailed:
		if rf.AutoCompensate {
			// 自动补偿：标记 failed 但安排下次重试
			_, _ = w.refundSvc.MarkFailed(ctx, rf.PaymentIntentID, rf.ID, resp.FailureMessage)
			w.scheduleNext(ctx, rf, resp.FailureMessage)
		} else {
			_, _ = w.refundSvc.MarkFailed(ctx, rf.PaymentIntentID, rf.ID, resp.FailureMessage)
		}
	}
}

// scheduleNext 指数退避安排下次重试
func (w *RefundRetryWorker) scheduleNext(ctx context.Context, rf *domain.Refund, reason string) {
	// 指数退避：min(60 * 2^retry_count, 1h)；auto_compensate=1 永不放弃
	backoffSec := 60 * int64(math.Pow(2, math.Min(float64(rf.RetryCount), 6))) // 上限 60*64 = 64min
	if backoffSec > 3600 {
		backoffSec = 3600
	}
	next := time.Now().UTC().Add(time.Duration(backoffSec) * time.Second)
	_, err := w.refundRepo.UpdateFields(ctx, rf.PaymentIntentID, rf.ID, map[string]any{
		"retry_count":   rf.RetryCount + 1,
		"next_retry_at": next,
		"failure_reason": reason,
	})
	if err != nil {
		w.logger.Warn("schedule next retry failed", zap.Error(err), zap.String("refund_id", rf.ID))
	}
}

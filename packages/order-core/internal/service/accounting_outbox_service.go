package service

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
)

// AccountingOutboxService 把 "charge/refund 已成交" 事件落到分片 outbox，
// 供 accounting_outbox_worker 异步投递给 accounting-system。
//
// 调用链：webhook_service 成功分支 → EnqueueChargeSucceeded / EnqueueRefundSucceeded。
// 幂等：同一 (pi, event, charge/refund) 只入队一次（repo 按 request_id 唯一约束拦截重复）。
// 路由决策：
//   - PI.CustomerID != ""               → owner=user,  USER_BALANCE  方向
//   - PI.CustomerID == "" && MchID!=""  → owner=mch,   MERCHANT_PENDING_SETTLE 方向
//   - 两者皆空                          → skip（log info）
type AccountingOutboxService interface {
	EnqueueChargeSucceeded(ctx context.Context, pi *domain.PaymentIntent, chargeID string, amount int64) error
	EnqueueRefundSucceeded(ctx context.Context, pi *domain.PaymentIntent, rf *domain.Refund) error
}

// OutboxWaker 让 service 在写完 outbox 后 nudge worker 立刻跑一轮。
// AccountingOutboxWorker 实现了这个接口。
type OutboxWaker interface {
	Wake()
}

type accountingOutboxService struct {
	outboxRepo repo.AccountingOutboxRepository
	idg        idgen.IDGenerator
	logger     *zap.Logger

	// inlineClient 非 nil 时，enqueue 成功后尝试同步投递一次（best-effort）：
	//   - 成功 → 直接 MarkSent，省掉 worker 5s 轮询
	//   - 失败 → 仅记 warn，行保持 pending，由 worker 继续重试
	// 这条快路径只是延迟优化，outbox 依旧是可靠性兜底。
	inlineClient AccountingClient

	// waker 非 nil 时，Insert 后且 inline 未成功（或未启用）时 nudge worker 立刻 Tick，
	// 把行投递延迟从"最坏 poll interval"（5s）压到毫秒级；即使 inline 不可用
	// （webhook 重试链路等）也不会等满 5s。
	waker OutboxWaker
}

// NewAccountingOutboxService 构造。
func NewAccountingOutboxService(outboxRepo repo.AccountingOutboxRepository, idg idgen.IDGenerator, logger *zap.Logger) AccountingOutboxService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &accountingOutboxService{outboxRepo: outboxRepo, idg: idg, logger: logger}
}

// SetInlineClient 注入同步投递使用的 accounting client。传 nil 可关闭快路径。
// 仅在启动期调用一次；运行期不做切换。
func (s *accountingOutboxService) SetInlineClient(c AccountingClient) {
	s.inlineClient = c
}

// SetWaker 注入 worker wake 器。仅在启动期调用一次。
func (s *accountingOutboxService) SetWaker(w OutboxWaker) {
	s.waker = w
}

// EnqueueChargeSucceeded charge 成功后入队。
func (s *accountingOutboxService) EnqueueChargeSucceeded(ctx context.Context, pi *domain.PaymentIntent, chargeID string, amount int64) error {
	if pi == nil || pi.ID == "" {
		return fmt.Errorf("%w: pi required", domain.ErrValidation)
	}
	row := s.buildChargeRow(pi, chargeID, amount)
	if row == nil {
		s.logger.Info("accounting outbox skipped: no owner on PI",
			zap.String("pi_id", pi.ID),
			zap.String("charge_id", chargeID))
		return nil
	}
	return s.enqueue(ctx, row)
}

// EnqueueRefundSucceeded refund 成功后入队。
func (s *accountingOutboxService) EnqueueRefundSucceeded(ctx context.Context, pi *domain.PaymentIntent, rf *domain.Refund) error {
	if pi == nil || pi.ID == "" || rf == nil || rf.ID == "" {
		return fmt.Errorf("%w: pi and refund required", domain.ErrValidation)
	}
	row := s.buildRefundRow(pi, rf)
	if row == nil {
		s.logger.Info("accounting outbox skipped: no owner on PI",
			zap.String("pi_id", pi.ID),
			zap.String("refund_id", rf.ID))
		return nil
	}
	return s.enqueue(ctx, row)
}

func (s *accountingOutboxService) buildChargeRow(pi *domain.PaymentIntent, chargeID string, amount int64) *domain.AccountingOutbox {
	base := &domain.AccountingOutbox{
		RequestID:       fmt.Sprintf("%s:charge_succeeded:%s", pi.ID, chargeID),
		EventType:       domain.AccountingEventChargeSucceeded,
		PaymentIntentID: pi.ID,
		ChargeID:        chargeID,
		PaymentMethod:   pi.PaymentMethod,
		Amount:          amount,
		Currency:        pi.Currency,
		Metadata:        domain.Metadata{"mch_id": pi.MchID},
	}
	switch {
	case pi.CustomerID != "":
		base.OwnerType = domain.AccountingOwnerUser
		base.OwnerID = pi.CustomerID
	case pi.MchID != "":
		base.OwnerType = domain.AccountingOwnerMerchant
		base.OwnerID = pi.MchID
	default:
		return nil
	}
	return base
}

func (s *accountingOutboxService) buildRefundRow(pi *domain.PaymentIntent, rf *domain.Refund) *domain.AccountingOutbox {
	currency := rf.Currency
	if currency == "" {
		currency = pi.Currency
	}
	base := &domain.AccountingOutbox{
		RequestID:       fmt.Sprintf("%s:refund_succeeded:%s", pi.ID, rf.ID),
		EventType:       domain.AccountingEventRefundSucceeded,
		PaymentIntentID: pi.ID,
		ChargeID:        rf.ChargeID,
		RefundID:        rf.ID,
		PaymentMethod:   pi.PaymentMethod,
		Amount:          rf.Amount,
		Currency:        currency,
		Metadata:        domain.Metadata{"mch_id": pi.MchID},
	}
	switch {
	case pi.CustomerID != "":
		base.OwnerType = domain.AccountingOwnerUser
		base.OwnerID = pi.CustomerID
	case pi.MchID != "":
		base.OwnerType = domain.AccountingOwnerMerchant
		base.OwnerID = pi.MchID
	default:
		return nil
	}
	return base
}

func (s *accountingOutboxService) enqueue(ctx context.Context, row *domain.AccountingOutbox) error {
	seq, err := s.idg.NextID(ctx, idgen.BizTagAccountingOutbox)
	if err != nil {
		return fmt.Errorf("idgen for accounting outbox: %w", err)
	}
	row.ID = fmt.Sprintf("aob_%d", seq)
	_, err = s.outboxRepo.Insert(ctx, row)
	switch {
	case err == nil:
		s.logger.Info("accounting outbox enqueued",
			zap.String("request_id", row.RequestID),
			zap.String("pi_id", row.PaymentIntentID),
			zap.String("owner_type", string(row.OwnerType)),
			zap.String("payment_method", row.PaymentMethod),
			zap.Int64("amount", row.Amount))
		s.tryInlineDeliver(ctx, row)
		return nil
	case errors.Is(err, domain.ErrAccountingOutboxDuplicate):
		s.logger.Debug("accounting outbox duplicate, idempotent",
			zap.String("request_id", row.RequestID))
		return nil
	default:
		return err
	}
}

// tryInlineDeliver 行刚入队成功后同步投递一次。失败不返回错误：调用方已经把
// 行写入 outbox，worker 会兜底重试；我们只是尝试把常见的成功路径压到 ms 级。
//
// 崩溃恢复：
//   - 行已先 Insert 到分片 outbox 表（durable）
//   - DoubleEntryBooking 在 RPC 中途崩溃 → 行仍为 pending → worker Tick 重发
//   - DoubleEntryBooking 成功但 MarkSent 前崩溃 → worker 重发；accounting-system
//     按 request_id 幂等返回首次结果（不会重复记账），worker 再 MarkSent
//   - 进程在 tryInlineDeliver 入口之前就被 SIGKILL → 行仍为 pending → worker 兜底
// 以上所有路径最终一致；outbox 是可靠性底座，inline 只是延迟优化。
func (s *accountingOutboxService) tryInlineDeliver(ctx context.Context, row *domain.AccountingOutbox) {
	if s.inlineClient == nil {
		// inline 关闭（endpoint 未配置 / webhook 重试链路等）。行留给 worker；
		// 立刻唤醒避免 5s poll 滞后。
		s.wakeWorker()
		return
	}
	if err := s.inlineClient.DoubleEntryBooking(ctx, row); err != nil {
		s.logger.Warn("accounting inline delivery failed; worker will retry",
			zap.String("id", row.ID),
			zap.String("request_id", row.RequestID),
			zap.Error(err))
		s.wakeWorker()
		return
	}
	if err := s.outboxRepo.MarkSent(ctx, row); err != nil {
		s.logger.Warn("accounting inline delivery: mark sent failed; worker will re-send (idempotent via request_id)",
			zap.String("id", row.ID),
			zap.Error(err))
		s.wakeWorker()
		return
	}
	s.logger.Debug("accounting inline delivery succeeded",
		zap.String("id", row.ID),
		zap.String("request_id", row.RequestID))
}

func (s *accountingOutboxService) wakeWorker() {
	if s.waker != nil {
		s.waker.Wake()
	}
}

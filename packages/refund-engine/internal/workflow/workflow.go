// Package workflow — refund 工作流核心。
//
//   Request          创建退款请求 (幂等 by idempotency_key)
//   Approve / Reject ops 复核（大额）
//   Submit           cron 把 approved 状态的退款提交通道
//   MarkCompleted    通道 webhook 回调成功
//   MarkFailed       通道返回错或网络错

package workflow

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/refund-engine/internal/domain"
)

// Repository 抽象。
type Repository interface {
	Create(ctx context.Context, r *domain.Refund) (int64, error)
	Get(ctx context.Context, id int64) (*domain.Refund, error)
	GetByRefundID(ctx context.Context, refundID string) (*domain.Refund, error)
	GetByIdempotency(ctx context.Context, key string) (*domain.Refund, error)
	UpdateStatus(ctx context.Context, id int64, to domain.Status, fields map[string]any) error
	ListByCharge(ctx context.Context, chargeID string) ([]*domain.Refund, error)
	ListByStatus(ctx context.Context, status domain.Status, limit int) ([]*domain.Refund, error)
	SumRefundedByCharge(ctx context.Context, chargeID string) (int64, error)
}

// ChannelClient 抽象通道退款 API。
type ChannelClient interface {
	SubmitRefund(ctx context.Context, r *domain.Refund) (channelRefundID string, err error)
}

// Notifier 出站事件 — 接 merchant-webhook + billing-system + accounting。
type Notifier interface {
	NotifyMerchant(ctx context.Context, merchantID, eventType string, payload any) error
	NotifyBilling(ctx context.Context, r *domain.Refund) error
}

// Service 主对外接口。
type Service struct {
	repo     Repository
	channel  ChannelClient
	notifier Notifier
	log      *zap.Logger
	approvalThresholdMinor int64
	mu       sync.Mutex
	counter  int64
}

// New 构造。
func New(repo Repository, ch ChannelClient, notifier Notifier, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		repo: repo, channel: ch, notifier: notifier, log: log,
		approvalThresholdMinor: 100_000, // $1k 以上需 ops 复核
	}
}

// RefundRequest 入参。
type RefundRequest struct {
	MerchantID                string
	ChargeID                  string
	PaymentIntentID           string
	AmountMinor               int64
	OriginalChargeAmountMinor int64
	Currency                  string
	Reason                    domain.ReasonCode
	ReasonNote                string
	Method                    domain.RefundMethod
	IdempotencyKey            string
	RequestedBy               string // 'customer'/'merchant'/'ops'/'chargeback'
	TraceID                   string
}

// Request 创建退款请求。
//
// 校验:
//   1. 幂等 by idempotency_key (重复请求返已有)
//   2. AmountMinor > 0 & <= original_charge - already_refunded
//   3. method 兜底 original_channel
//
// 状态:
//   - 小额（< approvalThresholdMinor）+ requested_by != customer → 直接 approved
//   - 大额 / customer 触发 → requested 等 ops 审核
func (s *Service) Request(ctx context.Context, req RefundRequest) (*domain.Refund, error) {
	if req.MerchantID == "" || req.ChargeID == "" {
		return nil, fmt.Errorf("merchant_id and charge_id required")
	}
	if req.AmountMinor <= 0 {
		return nil, fmt.Errorf("amount_minor must be > 0")
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = computeIdemp(req.MerchantID, req.ChargeID, req.AmountMinor, req.RequestedBy)
	}
	// 幂等 check
	if existing, _ := s.repo.GetByIdempotency(ctx, req.IdempotencyKey); existing != nil {
		s.log.Info("refund idempotent hit",
			zap.String("idempotency_key", req.IdempotencyKey),
			zap.Int64("existing_id", existing.ID))
		return existing, nil
	}
	// 累计退款 check
	alreadyRefunded, err := s.repo.SumRefundedByCharge(ctx, req.ChargeID)
	if err != nil {
		return nil, fmt.Errorf("sum refunded: %w", err)
	}
	if req.OriginalChargeAmountMinor > 0 &&
		alreadyRefunded+req.AmountMinor > req.OriginalChargeAmountMinor {
		return nil, fmt.Errorf("refund excess: already=%d + new=%d > original=%d",
			alreadyRefunded, req.AmountMinor, req.OriginalChargeAmountMinor)
	}
	if req.Method == "" {
		req.Method = domain.MethodOriginalChannel
	}
	// 决定初始 status
	status := domain.StatusApproved
	if req.AmountMinor >= s.approvalThresholdMinor || req.RequestedBy == "customer" {
		status = domain.StatusRequested
	}
	// 'chargeback' 来源（dispute-service 触发）自动 approve
	if req.RequestedBy == "chargeback" {
		status = domain.StatusApproved
	}

	s.mu.Lock()
	s.counter++
	refundID := fmt.Sprintf("re_%d_%d", time.Now().UnixMilli(), s.counter)
	s.mu.Unlock()

	now := time.Now().UTC()
	r := &domain.Refund{
		RefundID:                  refundID,
		MerchantID:                req.MerchantID,
		ChargeID:                  req.ChargeID,
		PaymentIntentID:           req.PaymentIntentID,
		AmountMinor:               req.AmountMinor,
		OriginalChargeAmountMinor: req.OriginalChargeAmountMinor,
		AlreadyRefundedMinor:      alreadyRefunded,
		Currency:                  req.Currency,
		Reason:                    req.Reason,
		ReasonNote:                req.ReasonNote,
		Method:                    req.Method,
		Status:                    status,
		IdempotencyKey:            req.IdempotencyKey,
		RequestedBy:               req.RequestedBy,
		RequestedAt:               now,
		TraceID:                   req.TraceID,
		CreatedAt:                 now,
		UpdatedAt:                 now,
	}
	id, err := s.repo.Create(ctx, r)
	if err != nil {
		return nil, err
	}
	r.ID = id
	s.log.Info("refund created",
		zap.Int64("id", id), zap.String("refund_id", refundID),
		zap.Int64("amount_minor", req.AmountMinor), zap.String("status", string(status)))
	// 通知商户 refund.created（不论 approved 还是 requested 都通知）
	if s.notifier != nil {
		_ = s.notifier.NotifyMerchant(ctx, req.MerchantID, "refund.created", r)
	}
	return r, nil
}

// Approve ops 复核大额退款。
func (s *Service) Approve(ctx context.Context, id int64, by string) error {
	r, err := s.repo.Get(ctx, id)
	if err != nil || r == nil {
		return fmt.Errorf("refund not found")
	}
	if !domain.ValidTransition(r.Status, domain.StatusApproved) {
		return fmt.Errorf("invalid transition %s → approved", r.Status)
	}
	now := time.Now().UTC()
	return s.repo.UpdateStatus(ctx, id, domain.StatusApproved, map[string]any{
		"approved_by": by, "approved_at": &now, "updated_at": now,
	})
}

// Reject ops 拒绝退款（疑似欺诈等）。
func (s *Service) Reject(ctx context.Context, id int64, by, reason string) error {
	now := time.Now().UTC()
	return s.repo.UpdateStatus(ctx, id, domain.StatusRejected, map[string]any{
		"approved_by": by, "approved_at": &now,
		"failure_message": reason, "updated_at": now,
	})
}

// Submit cron 跑 approved → 调通道 → submitted。
func (s *Service) Submit(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	approved, err := s.repo.ListByStatus(ctx, domain.StatusApproved, limit)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, r := range approved {
		channelRef, err := s.channel.SubmitRefund(ctx, r)
		if err != nil {
			s.markFailed(ctx, r.ID, "channel_error", err.Error())
			continue
		}
		now := time.Now().UTC()
		s.repo.UpdateStatus(ctx, r.ID, domain.StatusSubmitted, map[string]any{
			"channel_refund_id": channelRef,
			"submitted_at":      &now,
			"updated_at":        now,
		})
		sent++
	}
	return sent, nil
}

// MarkCompleted 通道 webhook 回调成功。
func (s *Service) MarkCompleted(ctx context.Context, refundID, channelRef string) error {
	r, err := s.repo.GetByRefundID(ctx, refundID)
	if err != nil || r == nil {
		return fmt.Errorf("refund %s not found", refundID)
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateStatus(ctx, r.ID, domain.StatusCompleted, map[string]any{
		"channel_refund_id": channelRef,
		"completed_at":      &now,
		"updated_at":        now,
	}); err != nil {
		return err
	}
	// 完成后:
	//  1. 通知 billing 写 refund fee_event（fee 按 rule 退还/留存）
	//  2. 通知 accounting 调账户余额
	//  3. 通知商户 refund.completed
	r.Status = domain.StatusCompleted
	if s.notifier != nil {
		_ = s.notifier.NotifyBilling(ctx, r)
		_ = s.notifier.NotifyMerchant(ctx, r.MerchantID, "refund.completed", r)
	}
	return nil
}

// MarkFailed 通道返回失败 / 重试用尽。
func (s *Service) MarkFailed(ctx context.Context, refundID, code, msg string) error {
	r, err := s.repo.GetByRefundID(ctx, refundID)
	if err != nil || r == nil {
		return fmt.Errorf("refund %s not found", refundID)
	}
	if err := s.markFailed(ctx, r.ID, code, msg); err != nil {
		return err
	}
	if s.notifier != nil {
		_ = s.notifier.NotifyMerchant(ctx, r.MerchantID, "refund.failed", r)
	}
	return nil
}

func (s *Service) markFailed(ctx context.Context, id int64, code, msg string) error {
	return s.repo.UpdateStatus(ctx, id, domain.StatusFailed, map[string]any{
		"failure_code":    code,
		"failure_message": msg,
		"updated_at":      time.Now().UTC(),
	})
}

func computeIdemp(merchantID, chargeID string, amount int64, requestedBy string) string {
	h := sha1.New()
	h.Write([]byte(merchantID))
	h.Write([]byte{0})
	h.Write([]byte(chargeID))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", amount)
	h.Write([]byte{0})
	h.Write([]byte(requestedBy))
	return hex.EncodeToString(h.Sum(nil)[:12])
}

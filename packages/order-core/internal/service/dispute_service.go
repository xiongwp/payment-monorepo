// Package service — dispute / chargeback orchestration.
package service

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/idgen"
	"github.com/xiongwp/order-core/internal/repo"
)

// DisputeService lifecycle management for disputes.
//
// Events come from two sources:
//   - channel_webhook: dispute.* events forwarded by payment-channel →
//     WebhookService.Ingest → dispute.OpenFromWebhook / UpdateFromWebhook
//   - admin: ops/merchant manual actions (submit evidence, concede, etc.)
//
// Side-effects:
//   - "lost" or "charge_refunded" with AutoRefundCharge=true triggers an
//     automatic refund of the disputed amount on the original charge, posted
//     through RefundService so the existing FSM + ledger hooks run.
type DisputeService interface {
	// Open creates a new dispute (usually from channel webhook).
	// If a dispute with the same (channel, channel_dispute_id) already exists,
	// returns it unchanged — idempotent.
	Open(ctx context.Context, in *OpenDisputeInput) (*domain.Dispute, error)

	Get(ctx context.Context, piID, id string) (*domain.Dispute, error)
	ListByPI(ctx context.Context, piID string) ([]*domain.Dispute, error)
	ListByMerchant(ctx context.Context, merchantID string, status domain.DisputeStatus, limit, offset int) ([]*domain.Dispute, int64, error)
	ListEvents(ctx context.Context, piID, disputeID string, limit int) ([]*domain.DisputeEvent, error)

	// Admin transitions (each checks valid sources)
	SubmitEvidence(ctx context.Context, piID, id, actor string, evidence domain.Metadata) (*domain.Dispute, error)
	Concede(ctx context.Context, piID, id, actor, note string) (*domain.Dispute, error) // → charge_refunded
	Cancel(ctx context.Context, piID, id, actor, note string) (*domain.Dispute, error)

	// Channel-webhook driven
	MarkUnderReview(ctx context.Context, piID, id string, payload domain.Metadata) (*domain.Dispute, error)
	MarkWon(ctx context.Context, piID, id string, payload domain.Metadata) (*domain.Dispute, error)
	MarkLost(ctx context.Context, piID, id string, outcomeAmount int64, payload domain.Metadata) (*domain.Dispute, error)
	MarkWarningClosed(ctx context.Context, piID, id string, payload domain.Metadata) (*domain.Dispute, error)
}

// OpenDisputeInput bootstraps a new dispute row.
type OpenDisputeInput struct {
	PaymentIntentID  string
	ChargeID         string
	MerchantID       string
	Channel          string
	ChannelDisputeID string
	Amount           int64
	Currency         string
	Reason           domain.DisputeReason
	ReasonDetail     string
	EvidenceDueAt    *time.Time
	AutoRefundCharge bool
	Metadata         domain.Metadata
	Source           string // channel_webhook / admin / system
	Payload          domain.Metadata
}

type disputeService struct {
	repo   repo.DisputeRepository
	idg    idgen.IDGenerator
	logger *zap.Logger

	// Optional: when set, lost / charge_refunded triggers a refund through the
	// existing RefundService so PI.refund_phase + NotifyLog + ledger hooks run.
	rfSvc RefundService
}

// NewDisputeService 构造
func NewDisputeService(r repo.DisputeRepository, g idgen.IDGenerator, rfSvc RefundService, logger *zap.Logger) DisputeService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &disputeService{repo: r, idg: g, rfSvc: rfSvc, logger: logger}
}

// ─── Open ───────────────────────────────────────────────────────────────────

func (s *disputeService) Open(ctx context.Context, in *OpenDisputeInput) (*domain.Dispute, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: input nil", domain.ErrValidation)
	}
	if in.PaymentIntentID == "" || in.ChargeID == "" || in.Channel == "" {
		return nil, fmt.Errorf("%w: pi_id/charge_id/channel required", domain.ErrValidation)
	}
	if in.Amount <= 0 {
		return nil, fmt.Errorf("%w: amount must be > 0", domain.ErrValidation)
	}
	// Idempotent: same (channel, channel_dispute_id) → return existing.
	if in.ChannelDisputeID != "" {
		if existing, err := s.repo.GetByChannelDisputeID(ctx, in.PaymentIntentID, in.Channel, in.ChannelDisputeID); err == nil {
			return existing, nil
		}
	}
	seq, err := s.idg.NextID(ctx, idgen.BizTagDispute)
	if err != nil {
		return nil, fmt.Errorf("idgen: %w", err)
	}
	d := &domain.Dispute{
		ID:               fmt.Sprintf("dp_%d", seq),
		PaymentIntentID:  in.PaymentIntentID,
		ChargeID:         in.ChargeID,
		MerchantID:       in.MerchantID,
		Channel:          in.Channel,
		ChannelDisputeID: in.ChannelDisputeID,
		Status:           domain.DisputeNeedsResponse,
		Amount:           in.Amount,
		Currency:         firstNonEmptyStr(in.Currency, "PHP"),
		Reason:           in.Reason,
		ReasonDetail:     in.ReasonDetail,
		EvidenceDueAt:    in.EvidenceDueAt,
		AutoRefundCharge: in.AutoRefundCharge,
		Metadata:         in.Metadata,
	}
	if err := s.repo.Create(ctx, d); err != nil {
		return nil, err
	}
	// Initial event row is optional; skipped to keep the Create tx simple.
	// First real transition (→ under_review / canceled / ...) will be the
	// earliest event in the audit trail.
	s.logger.Info("dispute opened",
		zap.String("id", d.ID),
		zap.String("pi_id", d.PaymentIntentID),
		zap.String("channel", d.Channel),
		zap.String("reason", string(d.Reason)))
	return d, nil
}

// ─── Reads ──────────────────────────────────────────────────────────────────

func (s *disputeService) Get(ctx context.Context, piID, id string) (*domain.Dispute, error) {
	return s.repo.Get(ctx, piID, id)
}
func (s *disputeService) ListByPI(ctx context.Context, piID string) ([]*domain.Dispute, error) {
	return s.repo.ListByPI(ctx, piID)
}
func (s *disputeService) ListByMerchant(ctx context.Context, merchantID string, status domain.DisputeStatus, limit, offset int) ([]*domain.Dispute, int64, error) {
	return s.repo.ListByMerchant(ctx, merchantID, status, limit, offset)
}
func (s *disputeService) ListEvents(ctx context.Context, piID, disputeID string, limit int) ([]*domain.DisputeEvent, error) {
	return s.repo.ListEvents(ctx, piID, disputeID, limit)
}

// ─── Transitions ────────────────────────────────────────────────────────────

func (s *disputeService) transition(ctx context.Context, piID, id, source, actor, note string, to domain.DisputeStatus, payload domain.Metadata, validFrom ...domain.DisputeStatus) (*domain.Dispute, error) {
	cur, err := s.repo.Get(ctx, piID, id)
	if err != nil {
		return nil, err
	}
	if len(validFrom) > 0 {
		ok := false
		for _, v := range validFrom {
			if cur.Status == v {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("%w: current=%s, expected one of %v", domain.ErrDisputeInvalidTransition, cur.Status, validFrom)
		}
	}
	evt := &domain.DisputeEvent{
		Source:  source,
		Actor:   actor,
		Note:    note,
		Payload: payload,
	}
	return s.repo.Transition(ctx, piID, id, cur.Status, to, evt)
}

func (s *disputeService) SubmitEvidence(ctx context.Context, piID, id, actor string, evidence domain.Metadata) (*domain.Dispute, error) {
	// Save evidence payload first, then transition to under_review.
	if _, err := s.repo.UpdateFields(ctx, piID, id, map[string]any{"evidence": evidence}); err != nil {
		return nil, err
	}
	return s.transition(ctx, piID, id, "admin", actor, "evidence submitted",
		domain.DisputeUnderReview, evidence,
		domain.DisputeNeedsResponse, domain.DisputeUnderReview)
}

func (s *disputeService) Concede(ctx context.Context, piID, id, actor, note string) (*domain.Dispute, error) {
	d, err := s.transition(ctx, piID, id, "admin", actor, note,
		domain.DisputeChargeRefunded, nil,
		domain.DisputeNeedsResponse, domain.DisputeUnderReview)
	if err != nil {
		return nil, err
	}
	s.triggerAutoRefund(ctx, d, actor, "dispute conceded")
	return d, nil
}

func (s *disputeService) Cancel(ctx context.Context, piID, id, actor, note string) (*domain.Dispute, error) {
	return s.transition(ctx, piID, id, "admin", actor, note,
		domain.DisputeCanceled, nil,
		domain.DisputeNeedsResponse, domain.DisputeUnderReview)
}

func (s *disputeService) MarkUnderReview(ctx context.Context, piID, id string, payload domain.Metadata) (*domain.Dispute, error) {
	return s.transition(ctx, piID, id, "channel_webhook", "", "", domain.DisputeUnderReview, payload,
		domain.DisputeNeedsResponse)
}

func (s *disputeService) MarkWon(ctx context.Context, piID, id string, payload domain.Metadata) (*domain.Dispute, error) {
	d, err := s.transition(ctx, piID, id, "channel_webhook", "", "", domain.DisputeWon, payload,
		domain.DisputeUnderReview, domain.DisputeNeedsResponse)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, _ = s.repo.UpdateFields(ctx, piID, id, map[string]any{"decided_at": now})
	return d, nil
}

func (s *disputeService) MarkLost(ctx context.Context, piID, id string, outcomeAmount int64, payload domain.Metadata) (*domain.Dispute, error) {
	d, err := s.transition(ctx, piID, id, "channel_webhook", "", "", domain.DisputeLost, payload,
		domain.DisputeUnderReview, domain.DisputeNeedsResponse)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, _ = s.repo.UpdateFields(ctx, piID, id, map[string]any{
		"decided_at":     now,
		"outcome_amount": outcomeAmount,
	})
	s.triggerAutoRefund(ctx, d, "system", "dispute lost — auto refund")
	return d, nil
}

func (s *disputeService) MarkWarningClosed(ctx context.Context, piID, id string, payload domain.Metadata) (*domain.Dispute, error) {
	return s.transition(ctx, piID, id, "channel_webhook", "", "", domain.DisputeWarningClosed, payload,
		domain.DisputeNeedsResponse)
}

// triggerAutoRefund fires a refund through RefundService when the dispute's
// outcome calls for it. Silent no-op if AutoRefundCharge is false or
// RefundService is not wired.
func (s *disputeService) triggerAutoRefund(ctx context.Context, d *domain.Dispute, actor, reason string) {
	if !d.AutoRefundCharge || s.rfSvc == nil {
		return
	}
	amount := d.Amount
	if d.OutcomeAmount != nil && *d.OutcomeAmount > 0 {
		amount = *d.OutcomeAmount
	}
	_, err := s.rfSvc.Create(ctx, &CreateRefundInput{
		PaymentIntentID: d.PaymentIntentID,
		ChargeID:        d.ChargeID,
		Amount:          amount,
		Reason:          domain.RefundReasonFraudulent, // dispute-driven refund
		Metadata: map[string]string{
			"dispute_id":     d.ID,
			"dispute_reason": string(d.Reason),
			"dispute_actor":  actor,
			"dispute_note":   reason,
		},
	})
	if err != nil {
		s.logger.Warn("dispute auto-refund failed",
			zap.String("dispute_id", d.ID),
			zap.Error(err))
	}
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

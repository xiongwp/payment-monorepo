// Package workflow — dispute 状态机驱动 + business actions.
//
// 关键 actions:
//
//   IngestNetworkWebhook   信用卡组织发来 chargeback notification → 创建 Dispute
//                          + hold 商户余额 + 通知商户
//   SubmitEvidence         商户上传证据 (PDF/图片)
//   FinalizeMerchantResponse  商户提交完所有证据后 transition → 等卡组裁定
//   IngestRuling           卡组裁定结果回来 → won / lost
//   ExpireOverdue          cron 扫超过 deadline 还在 needs_response 的，
//                          自动 transition expired (= lost)
//   RecoverFunds           win 后释放 reserve hold 回 merchant
//
// 副作用:
//   - 状态变化 → 发商户 webhook (dispute.received / dispute.won / dispute.lost)
//   - won/lost 触发 clearing-settlement 调整 reserve / payout
//   - 失败 → reconplatform 写 anomaly diff

package workflow

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/dispute-service/internal/domain"
)

// Repository 抽象。
type Repository interface {
	Create(ctx context.Context, d *domain.Dispute) (int64, error)
	Get(ctx context.Context, id int64) (*domain.Dispute, error)
	GetByExternalID(ctx context.Context, externalID string) (*domain.Dispute, error)
	UpdateStatus(ctx context.Context, id int64, to domain.DisputeStatus, fields map[string]any) error
	ListByMerchant(ctx context.Context, merchantID string, status domain.DisputeStatus, limit int) ([]*domain.Dispute, error)
	ListOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Dispute, error)

	SaveEvidence(ctx context.Context, e *domain.Evidence) (int64, error)
	ListEvidence(ctx context.Context, disputeID int64) ([]*domain.Evidence, error)

	SaveEvent(ctx context.Context, e *domain.DisputeEvent) error
}

// Notifier 抽象 — 给商户发 webhook 等通知。生产接 merchant-webhook 包。
type Notifier interface {
	NotifyMerchant(ctx context.Context, merchantID, eventType string, payload any) error
}

// Service workflow 主对外接口。
type Service struct {
	repo       Repository
	notifier   Notifier
	log        *zap.Logger
	defaultDeadline time.Duration // 默认 deadline 7d (各卡组不同)
}

func New(repo Repository, notifier Notifier, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		repo:            repo,
		notifier:        notifier,
		log:             log,
		defaultDeadline: 7 * 24 * time.Hour,
	}
}

// IngestNetworkWebhook 卡组 chargeback notification → 创建 Dispute。
//
// 同 external_id 已存在 → 幂等返回。
func (s *Service) IngestNetworkWebhook(ctx context.Context, in domain.Dispute) (*domain.Dispute, error) {
	if in.ExternalID == "" {
		return nil, fmt.Errorf("external_id required")
	}
	if existing, _ := s.repo.GetByExternalID(ctx, in.ExternalID); existing != nil {
		s.log.Info("dispute idempotent hit",
			zap.String("external_id", in.ExternalID), zap.Int64("existing_id", existing.ID))
		return existing, nil
	}
	now := time.Now().UTC()
	in.Status = domain.StatusNeedsResponse
	in.ReceivedAt = now
	if in.ResponseDeadline.IsZero() {
		in.ResponseDeadline = now.Add(s.defaultDeadline)
	}
	in.CreatedAt = now
	in.UpdatedAt = now
	id, err := s.repo.Create(ctx, &in)
	if err != nil {
		return nil, fmt.Errorf("create dispute: %w", err)
	}
	in.ID = id
	// audit event
	_ = s.repo.SaveEvent(ctx, &domain.DisputeEvent{
		DisputeID: id, EventType: "received",
		From: "", To: domain.StatusNeedsResponse,
		Actor: "network",
		Details: fmt.Sprintf("Network=%s Reason=%s Amount=%d %s",
			in.Network, in.Reason, in.AmountMinor, in.Currency),
		CreatedAt: now,
	})
	// 通知商户 — webhook dispute.received
	if s.notifier != nil {
		_ = s.notifier.NotifyMerchant(ctx, in.MerchantID, "dispute.received", &in)
	}
	s.log.Info("dispute created",
		zap.Int64("id", id),
		zap.String("merchant_id", in.MerchantID),
		zap.String("reason", string(in.Reason)),
		zap.Time("deadline", in.ResponseDeadline))
	return &in, nil
}

// SubmitEvidence 商户上传一条证据。
func (s *Service) SubmitEvidence(ctx context.Context, disputeID int64, ev domain.Evidence) (int64, error) {
	d, err := s.repo.Get(ctx, disputeID)
	if err != nil {
		return 0, err
	}
	if d.Status != domain.StatusNeedsResponse {
		return 0, fmt.Errorf("dispute %d not accepting evidence (status=%s)", disputeID, d.Status)
	}
	ev.DisputeID = disputeID
	ev.CreatedAt = time.Now().UTC()
	id, err := s.repo.SaveEvidence(ctx, &ev)
	if err != nil {
		return 0, err
	}
	s.log.Info("evidence submitted",
		zap.Int64("dispute_id", disputeID),
		zap.Int64("evidence_id", id),
		zap.String("type", ev.Type))
	return id, nil
}

// FinalizeMerchantResponse 商户表示证据都提交完了 → status: needs_response → evidence_submitted。
func (s *Service) FinalizeMerchantResponse(ctx context.Context, disputeID int64, by string) error {
	d, err := s.repo.Get(ctx, disputeID)
	if err != nil {
		return err
	}
	if !domain.ValidTransition(d.Status, domain.StatusEvidenceSubmitted) {
		return fmt.Errorf("invalid transition %s → evidence_submitted", d.Status)
	}
	// 必须至少有一条证据
	evs, _ := s.repo.ListEvidence(ctx, disputeID)
	if len(evs) == 0 {
		return fmt.Errorf("must submit at least one evidence item")
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateStatus(ctx, disputeID, domain.StatusEvidenceSubmitted,
		map[string]any{"evidence_submitted_at": &now, "updated_at": now}); err != nil {
		return err
	}
	_ = s.repo.SaveEvent(ctx, &domain.DisputeEvent{
		DisputeID: disputeID, EventType: "evidence_submitted",
		From: d.Status, To: domain.StatusEvidenceSubmitted,
		Actor: by, CreatedAt: now,
	})
	if s.notifier != nil {
		_ = s.notifier.NotifyMerchant(ctx, d.MerchantID, "dispute.evidence_submitted", d)
	}
	return nil
}

// IngestRuling 卡组裁定回来。outcome="won"|"lost"。
func (s *Service) IngestRuling(ctx context.Context, externalID, outcome string, details string) error {
	d, err := s.repo.GetByExternalID(ctx, externalID)
	if err != nil {
		return err
	}
	if d == nil {
		return fmt.Errorf("dispute %s not found", externalID)
	}
	var to domain.DisputeStatus
	switch outcome {
	case "won":
		to = domain.StatusWon
	case "lost":
		to = domain.StatusLost
	default:
		return fmt.Errorf("invalid outcome %q", outcome)
	}
	if !domain.ValidTransition(d.Status, to) {
		return fmt.Errorf("invalid transition %s → %s", d.Status, to)
	}
	now := time.Now().UTC()
	if err := s.repo.UpdateStatus(ctx, d.ID, to, map[string]any{
		"ruled_at":      &now,
		"ruled_outcome": outcome,
		"updated_at":    now,
	}); err != nil {
		return err
	}
	_ = s.repo.SaveEvent(ctx, &domain.DisputeEvent{
		DisputeID: d.ID, EventType: "ruled_" + outcome,
		From: d.Status, To: to, Actor: "network",
		Details: details, CreatedAt: now,
	})
	if s.notifier != nil {
		_ = s.notifier.NotifyMerchant(ctx, d.MerchantID, "dispute."+outcome, d)
	}
	s.log.Info("dispute ruled",
		zap.Int64("id", d.ID), zap.String("outcome", outcome))
	return nil
}

// ExpireOverdue cron 跑超期未 response 的 → expired (=lost)。
func (s *Service) ExpireOverdue(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	now := time.Now().UTC()
	overdue, err := s.repo.ListOverdue(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, d := range overdue {
		if err := s.repo.UpdateStatus(ctx, d.ID, domain.StatusExpired,
			map[string]any{"ruled_at": &now, "ruled_outcome": "lost", "updated_at": now}); err != nil {
			s.log.Warn("expire dispute failed",
				zap.Int64("id", d.ID), zap.Error(err))
			continue
		}
		_ = s.repo.SaveEvent(ctx, &domain.DisputeEvent{
			DisputeID: d.ID, EventType: "expired",
			From: d.Status, To: domain.StatusExpired,
			Actor: "system",
			Details: "auto-expired past response_deadline; treated as lost",
			CreatedAt: now,
		})
		if s.notifier != nil {
			_ = s.notifier.NotifyMerchant(ctx, d.MerchantID, "dispute.expired", d)
		}
		expired++
	}
	s.log.Info("dispute expire run", zap.Int("expired", expired))
	return expired, nil
}

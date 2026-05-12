// Package service — data-rights 业务编排层.
//
// canonical: handler (HTTP) → service (业务) → repo (GORM) → DB.
// service 持有 repo + audit + orchestrator + metrics, 不知道 fiber.

package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/audit"
	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/metrics"
	"reconcile-system/packages/data-rights/internal/orchestrator"
	"reconcile-system/packages/data-rights/internal/store"
)

type Service struct {
	store store.Store
	orch  *orchestrator.Orchestrator
	audit audit.Sink
	log   *zap.Logger
}

func New(s store.Store, orch *orchestrator.Orchestrator, audit audit.Sink, log *zap.Logger) *Service {
	return &Service{store: s, orch: orch, audit: audit, log: log}
}

func (s *Service) Submit(ctx context.Context, typ domain.RequestType, subj domain.Subject, jur domain.Jurisdiction, ip string) (domain.Request, error) {
	if typ == "" {
		typ = domain.RequestAccess
	}
	if jur == "" {
		jur = guessJurisdiction(subj.Country)
	}
	req := domain.Request{
		RequestID:    "dsar_" + randHex(16),
		Type:         typ,
		Subject:      subj,
		Jurisdiction: jur,
		State:        domain.StateReceived,
		SubmittedAt:  time.Now().UTC(),
		DeadlineAt:   time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if err := s.store.SaveRequest(req); err != nil {
		return domain.Request{}, err
	}
	metrics.RequestsTotal.WithLabelValues(string(typ), string(jur)).Inc()
	_ = s.audit.Emit(ctx, audit.Event{
		OccurredAt: req.SubmittedAt,
		Actor:      string(subj.Type) + ":" + subj.ID,
		Action:     "submit",
		RequestID:  req.RequestID,
		Details:    map[string]interface{}{"type": string(typ), "jurisdiction": string(jur)},
		SourceIP:   ip,
	})
	return req, nil
}

func (s *Service) Get(ctx context.Context, id string) (domain.Request, error) {
	return s.store.GetRequest(id)
}

func (s *Service) List(ctx context.Context, f store.ListFilter) ([]domain.Request, error) {
	return s.store.ListRequests(f)
}

func (s *Service) ParseListFilter(state, typ string, overdue bool, limit, offset int) store.ListFilter {
	return store.ListFilter{
		State:   domain.State(state),
		Type:    domain.RequestType(typ),
		Overdue: overdue,
		Limit:   limit,
		Offset:  offset,
	}
}

func (s *Service) Verify(ctx context.Context, id, reviewer, ip string) error {
	req, err := s.store.GetRequest(id)
	if err != nil {
		return err
	}
	req.Verification.Method = "ops_manual"
	req.Verification.VerifiedAt = time.Now().UTC()
	req.Verification.VerifierID = reviewer
	_ = s.store.SaveRequest(req)
	return s.store.UpdateState(id, domain.StateVerifying, reviewer)
}

func (s *Service) Approve(ctx context.Context, id, reviewer, ip string) error {
	if err := s.store.UpdateState(id, domain.StateCollecting, reviewer); err != nil {
		return err
	}
	// 异步 fan-out
	req, _ := s.store.GetRequest(id)
	go func() {
		fctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if req.Type == domain.RequestErasure {
			_ = s.orch.CollectErasure(fctx, req)
		} else {
			_ = s.orch.CollectAccess(fctx, req)
		}
		_ = s.store.UpdateState(id, domain.StateReview, "system")
	}()
	return nil
}

func (s *Service) Reject(ctx context.Context, id, reviewer, reason, ip string) error {
	return s.store.UpdateState(id, domain.StateRejected, reason)
}

func (s *Service) Fulfill(ctx context.Context, id, reviewer, ip string) error {
	latest, _ := s.store.GetRequest(id)
	sha := orchestrator.CombineExports(latest.ServiceStatuses)
	_ = s.store.SetExport(id, "s3://exports/"+id+".zip", sha)
	return s.store.UpdateState(id, domain.StateFulfilled, reviewer)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func guessJurisdiction(country string) domain.Jurisdiction {
	switch strings.ToUpper(country) {
	case "US":
		return domain.JurCA
	case "GB", "UK":
		return domain.JurUK
	case "BR":
		return domain.JurBR
	case "CN":
		return domain.JurCN
	case "DE", "FR", "IT", "ES", "NL", "BE", "PL", "SE", "DK", "FI", "AT", "IE":
		return domain.JurEU
	}
	return domain.JurOther
}

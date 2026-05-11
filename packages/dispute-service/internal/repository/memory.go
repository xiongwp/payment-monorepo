package repository

import (
	"context"
	"sort"
	"sync"
	"time"

	"reconcile-system/packages/dispute-service/internal/domain"
)

// MemoryRepo workflow.Repository in-mem 实现。
type MemoryRepo struct {
	mu        sync.RWMutex
	disputes  map[int64]*domain.Dispute
	byExt     map[string]*domain.Dispute
	evidence  map[int64][]*domain.Evidence
	events    []*domain.DisputeEvent
	nextDID   int64
	nextEID   int64
	nextEvtID int64
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		disputes: map[int64]*domain.Dispute{},
		byExt:    map[string]*domain.Dispute{},
		evidence: map[int64][]*domain.Evidence{},
	}
}

func (r *MemoryRepo) Create(ctx context.Context, d *domain.Dispute) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextDID++
	d.ID = r.nextDID
	r.disputes[d.ID] = d
	if d.ExternalID != "" {
		r.byExt[d.ExternalID] = d
	}
	return d.ID, nil
}

func (r *MemoryRepo) Get(ctx context.Context, id int64) (*domain.Dispute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d := r.disputes[id]
	if d == nil {
		return nil, nil
	}
	return d, nil
}

func (r *MemoryRepo) GetByExternalID(ctx context.Context, id string) (*domain.Dispute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byExt[id], nil
}

func (r *MemoryRepo) UpdateStatus(ctx context.Context, id int64, to domain.DisputeStatus, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.disputes[id]
	if d == nil {
		return nil
	}
	d.Status = to
	if v, ok := fields["ruled_at"].(*time.Time); ok && v != nil {
		d.RuledAt = v
	}
	if v, ok := fields["ruled_outcome"].(string); ok {
		d.RuledOutcome = v
	}
	if v, ok := fields["evidence_submitted_at"].(*time.Time); ok && v != nil {
		d.EvidenceSubmittedAt = v
	}
	if v, ok := fields["updated_at"].(time.Time); ok {
		d.UpdatedAt = v
	}
	return nil
}

func (r *MemoryRepo) ListByMerchant(ctx context.Context, merchantID string,
	status domain.DisputeStatus, limit int) ([]*domain.Dispute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.Dispute{}
	for _, d := range r.disputes {
		if d.MerchantID != merchantID {
			continue
		}
		if status != "" && d.Status != status {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *MemoryRepo) ListOverdue(ctx context.Context, now time.Time, limit int) ([]*domain.Dispute, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.Dispute{}
	for _, d := range r.disputes {
		if d.Status == domain.StatusNeedsResponse && now.After(d.ResponseDeadline) {
			out = append(out, d)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (r *MemoryRepo) SaveEvidence(ctx context.Context, e *domain.Evidence) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEID++
	e.ID = r.nextEID
	r.evidence[e.DisputeID] = append(r.evidence[e.DisputeID], e)
	return e.ID, nil
}

func (r *MemoryRepo) ListEvidence(ctx context.Context, disputeID int64) ([]*domain.Evidence, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.evidence[disputeID], nil
}

func (r *MemoryRepo) SaveEvent(ctx context.Context, e *domain.DisputeEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEvtID++
	e.ID = r.nextEvtID
	r.events = append(r.events, e)
	return nil
}

// LogNotifier 占位 Notifier — 仅打日志。
type LogNotifier struct{}

func (LogNotifier) NotifyMerchant(ctx context.Context, merchantID, eventType string, payload any) error {
	return nil // 真实接 merchant-webhook 包 dispatcher.Enqueue
}

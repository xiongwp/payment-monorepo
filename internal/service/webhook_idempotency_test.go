package service

import (
	"context"
	"errors"
	"testing"

	"github.com/xiongwp/order-core/internal/domain"
)

// TestInboundWebhook_DedupBehavior covers the contract that the production
// repo (InboundWebhookRepository) must satisfy and that the webhook service
// relies on:
//   - First (channel, event_id) Insert succeeds, returns the row
//   - Second Insert with the same key returns ErrInboundWebhookDuplicate +
//     the existing row
//   - Different event_id is independent
func TestInboundWebhook_DedupBehavior(t *testing.T) {
	repo := newMemInboundRepo()
	ctx := context.Background()

	w1 := &domain.InboundWebhook{ID: "in_1", ChannelName: "gcash", EventID: "evt_1", EventType: "charge.succeeded"}
	row, err := repo.Insert(ctx, w1)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if row.ID != "in_1" {
		t.Fatalf("first insert returned wrong row: %s", row.ID)
	}

	w1Dup := &domain.InboundWebhook{ID: "in_1_again", ChannelName: "gcash", EventID: "evt_1", EventType: "charge.succeeded"}
	dupRow, err := repo.Insert(ctx, w1Dup)
	if !errors.Is(err, domain.ErrInboundWebhookDuplicate) {
		t.Fatalf("duplicate insert should return ErrInboundWebhookDuplicate, got %v", err)
	}
	if dupRow.ID != "in_1" {
		t.Fatalf("duplicate insert should return original row id, got %s", dupRow.ID)
	}

	w2 := &domain.InboundWebhook{ID: "in_2", ChannelName: "gcash", EventID: "evt_2", EventType: "charge.failed"}
	if _, err := repo.Insert(ctx, w2); err != nil {
		t.Fatalf("different event_id should not collide: %v", err)
	}

	// Same event_id but different channel must NOT dedupe.
	w3 := &domain.InboundWebhook{ID: "in_3", ChannelName: "maya", EventID: "evt_1", EventType: "charge.succeeded"}
	if _, err := repo.Insert(ctx, w3); err != nil {
		t.Fatalf("same event_id on different channel should not collide: %v", err)
	}

	if got := len(repo.rows); got != 3 {
		t.Fatalf("rows = %d, want 3", got)
	}
}

// memInboundRepo: in-memory implementation of repo.InboundWebhookRepository.
// Test-only type that mirrors what the real GORM-backed repo does on a
// MySQL UNIQUE KEY (channel_name, event_id) violation.
type memInboundRepo struct {
	rows map[string]*domain.InboundWebhook
}

func newMemInboundRepo() *memInboundRepo {
	return &memInboundRepo{rows: map[string]*domain.InboundWebhook{}}
}

func keyOf(channel, eventID string) string { return channel + "|" + eventID }

func (m *memInboundRepo) Insert(_ context.Context, w *domain.InboundWebhook) (*domain.InboundWebhook, error) {
	k := keyOf(w.ChannelName, w.EventID)
	if existing, ok := m.rows[k]; ok {
		return existing, domain.ErrInboundWebhookDuplicate
	}
	cp := *w
	m.rows[k] = &cp
	return &cp, nil
}

func (m *memInboundRepo) GetByEvent(_ context.Context, channel, eventID, _ string) (*domain.InboundWebhook, error) {
	if r, ok := m.rows[keyOf(channel, eventID)]; ok {
		return r, nil
	}
	return nil, domain.ErrInboundWebhookDuplicate
}

func (m *memInboundRepo) MarkProcessed(_ context.Context, w *domain.InboundWebhook, status domain.InboundWebhookProcessStatus, _ string) error {
	if r, ok := m.rows[keyOf(w.ChannelName, w.EventID)]; ok {
		r.ProcessStatus = status
	}
	return nil
}

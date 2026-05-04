package session

import (
	"errors"
	"testing"
	"time"
)

func TestMemStore_CreateThenGet(t *testing.T) {
	m := NewMemStore()
	id, err := m.Create(Snapshot{FingerprintHash: "fp1", Platform: "ios"})
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 {
		t.Fatalf("session id should be 32 hex chars, got %d", len(id))
	}
	got := m.Get(id)
	if got == nil {
		t.Fatal("get returned nil")
	}
	if got.FingerprintHash != "fp1" || got.Platform != "ios" {
		t.Fatalf("fields mismatch: %+v", got)
	}
}

func TestMemStore_FinalizeUpdates(t *testing.T) {
	m := NewMemStore()
	id, _ := m.Create(Snapshot{FingerprintHash: "fp"})
	err := m.Finalize(id, BehaviorPatch{
		TimeToCheckoutMs: 5000,
		KeystrokeCount:   42,
		PastedFields:     []string{"card_number"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := m.Get(id)
	if got.TimeToCheckoutMs != 5000 || got.KeystrokeCount != 42 {
		t.Fatalf("not updated: %+v", got)
	}
	if len(got.PastedFields) != 1 || got.PastedFields[0] != "card_number" {
		t.Fatalf("pasted fields: %v", got.PastedFields)
	}
	// fingerprint 不变
	if got.FingerprintHash != "fp" {
		t.Fatal("fingerprint should not be cleared by finalize")
	}
}

func TestMemStore_FinalizeUnknownID(t *testing.T) {
	m := NewMemStore()
	err := m.Finalize("nope", BehaviorPatch{})
	var nf ErrNotFound
	if !errors.As(err, &nf) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestMemStore_GetUnknownReturnsNil(t *testing.T) {
	m := NewMemStore()
	if got := m.Get("nope"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

func TestMemStore_ExpiredSessionReturnsNil(t *testing.T) {
	m := NewMemStore()
	id, _ := m.Create(Snapshot{})
	// 强制把 CreatedAt 调到过期之外
	m.mu.Lock()
	m.items[id].CreatedAt = time.Now().Add(-time.Hour)
	m.mu.Unlock()
	if got := m.Get(id); got != nil {
		t.Fatal("expired session should be nil")
	}
}

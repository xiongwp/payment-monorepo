package featurestore

import (
	"context"
	"testing"
	"time"

	"github.com/xiongwp/risk-manage/internal/mlscore"
)

func mkSnap(id string) Snapshot {
	return Snapshot{
		DecisionID: id,
		OccurAt:    time.Now(),
		Features:   &mlscore.Features{Amount: 1000, Currency: "PHP"},
		Verdict:    "ALLOW",
	}
}

func TestMem_SaveAndGet(t *testing.T) {
	s := NewMemStore(0)
	s.Save(context.Background(), mkSnap("a"))
	got := s.Get("a")
	if got == nil || got.DecisionID != "a" {
		t.Fatalf("expected snapshot a, got %+v", got)
	}
}

func TestMem_SetOutcomeAfterSave(t *testing.T) {
	s := NewMemStore(0)
	s.Save(context.Background(), mkSnap("a"))
	s.SetOutcome(context.Background(), "a", true, time.Now())
	got := s.Get("a")
	if got == nil || got.OutcomeLabel == nil || *got.OutcomeLabel != true {
		t.Fatalf("outcome label not set: %+v", got)
	}
	if got.OutcomeAt == nil {
		t.Fatalf("outcome_at not set")
	}
}

func TestMem_SetOutcome_UnknownIDIsNoop(t *testing.T) {
	s := NewMemStore(0)
	// 不应 panic / 错
	s.SetOutcome(context.Background(), "unknown", true, time.Now())
}

func TestMem_FIFOEvictionWhenOverCapacity(t *testing.T) {
	s := NewMemStore(8) // small cap
	for i := 0; i < 12; i++ {
		s.Save(context.Background(), mkSnap(string(rune('a'+i))))
	}
	// 触发 eviction 后总量 ≤ max。具体保留哪些 map 迭代顺序不定，不强断言
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.m) > 8 {
		t.Fatalf("eviction didn't kick in; size=%d", len(s.m))
	}
}

func TestNoopStore_AllNoop(t *testing.T) {
	var s Store = NoopStore{}
	s.Save(context.Background(), mkSnap("x"))
	s.SetOutcome(context.Background(), "x", true, time.Now())
	if s.Get("x") != nil {
		t.Fatal("noop should always return nil")
	}
}

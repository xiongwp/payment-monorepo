package audit

import (
	"context"
	"testing"
)

func TestMemRuleAudit_WriteAndRecent(t *testing.T) {
	s := NewMemRuleAuditStore(0)
	for _, id := range []string{"a", "b", "c"} {
		_ = s.Write(context.Background(), RuleAuditEntry{
			Action: "mode_change", Actor: "alice", RuleID: id,
		})
	}
	got := s.Recent(0)
	if len(got) != 3 || got[0].RuleID != "c" {
		t.Fatalf("expected reverse-chrono [c b a]; got %+v", got)
	}
}

func TestMemRuleAudit_RingBufferOverflow(t *testing.T) {
	s := NewMemRuleAuditStore(3)
	for i := 0; i < 5; i++ {
		_ = s.Write(context.Background(), RuleAuditEntry{
			Action: "reload", Actor: "alice", RuleID: string(rune('a' + i)),
		})
	}
	got := s.Recent(0)
	if len(got) != 3 {
		t.Fatalf("ring should keep newest 3; got %d", len(got))
	}
	// 期望：e (最新), d, c
	if got[0].RuleID != "e" || got[1].RuleID != "d" || got[2].RuleID != "c" {
		t.Fatalf("unexpected order: %+v", []string{got[0].RuleID, got[1].RuleID, got[2].RuleID})
	}
}

func TestMemRuleAudit_ByRuleID(t *testing.T) {
	s := NewMemRuleAuditStore(0)
	_ = s.Write(context.Background(), RuleAuditEntry{Action: "reload", RuleID: "x", Actor: "alice"})
	_ = s.Write(context.Background(), RuleAuditEntry{Action: "mode_change", RuleID: "x", Actor: "bob"})
	_ = s.Write(context.Background(), RuleAuditEntry{Action: "reload", RuleID: "y", Actor: "alice"})
	got := s.ByRuleID("x", 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries for x; got %d", len(got))
	}
}

func TestMemRuleAudit_LimitClamp(t *testing.T) {
	s := NewMemRuleAuditStore(0)
	for i := 0; i < 10; i++ {
		_ = s.Write(context.Background(), RuleAuditEntry{Action: "reload", RuleID: "r"})
	}
	got := s.Recent(3)
	if len(got) != 3 {
		t.Fatalf("limit=3 expected 3; got %d", len(got))
	}
}

func TestMemRuleAudit_EmptyStoreSafe(t *testing.T) {
	s := NewMemRuleAuditStore(0)
	if got := s.Recent(0); len(got) != 0 {
		t.Fatal("empty store should return empty slice")
	}
	if got := s.ByRuleID("any", 0); len(got) != 0 {
		t.Fatal("empty store ByRuleID should return empty slice")
	}
	if got := s.ByRuleID("", 0); len(got) != 0 {
		t.Fatal("empty rule_id should return nil")
	}
}

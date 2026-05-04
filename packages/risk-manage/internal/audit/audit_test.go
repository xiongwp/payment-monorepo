package audit

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

func mkAudit(id string) *DecisionAudit {
	return &DecisionAudit{
		DecisionID:  id,
		OccurredAt:  time.Now().UTC(),
		Verdict:     "ALLOW",
		RiskScore:   0,
		RiskLevel:   "low",
	}
}

func mkAuditPI(id, pi string) *DecisionAudit {
	a := mkAudit(id)
	a.Input = AuditInput{PaymentIntentID: pi}
	return a
}

func TestMemSink_LookupByPaymentIntent(t *testing.T) {
	s := NewMemSink(8)
	s.Write(context.Background(), mkAuditPI("d1", "pi_a"))
	s.Write(context.Background(), mkAuditPI("d2", "pi_b"))
	if id, ok := s.LookupByPaymentIntent("pi_a"); !ok || id != "d1" {
		t.Fatalf("lookup pi_a: ok=%v id=%q", ok, id)
	}
	if _, ok := s.LookupByPaymentIntent("pi_missing"); ok {
		t.Fatal("missing PI should return ok=false")
	}
	if _, ok := s.LookupByPaymentIntent(""); ok {
		t.Fatal("empty PI should return ok=false")
	}
}

// PI 索引随 ring buffer 覆写自动清理，避免内存泄漏。
func TestMemSink_PIIndexCleansOnOverwrite(t *testing.T) {
	s := NewMemSink(2)
	s.Write(context.Background(), mkAuditPI("d1", "pi_a"))
	s.Write(context.Background(), mkAuditPI("d2", "pi_b"))
	s.Write(context.Background(), mkAuditPI("d3", "pi_c")) // 覆盖 d1
	if _, ok := s.LookupByPaymentIntent("pi_a"); ok {
		t.Fatal("pi_a should have been cleaned when d1 was overwritten")
	}
	if id, ok := s.LookupByPaymentIntent("pi_c"); !ok || id != "d3" {
		t.Fatalf("pi_c lookup failed: ok=%v id=%q", ok, id)
	}
}

func TestMemSink_LookupByDecisionID(t *testing.T) {
	s := NewMemSink(4)
	s.Write(context.Background(), mkAudit("d1"))
	s.Write(context.Background(), mkAudit("d2"))
	if a := s.LookupByDecisionID("d2"); a == nil || a.DecisionID != "d2" {
		t.Fatalf("lookup d2 failed: %+v", a)
	}
	if a := s.LookupByDecisionID("missing"); a != nil {
		t.Fatal("missing decision should return nil")
	}
}

func TestMemSink_RecentEmpty(t *testing.T) {
	s := NewMemSink(8)
	if got := s.Recent(10); len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}

func TestMemSink_WriteAndRecent(t *testing.T) {
	s := NewMemSink(8)
	for i := 0; i < 3; i++ {
		s.Write(context.Background(), mkAudit(string(rune('a'+i))))
	}
	got := s.Recent(10)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	// 最新写入的在最前面
	if got[0].DecisionID != "c" || got[1].DecisionID != "b" || got[2].DecisionID != "a" {
		t.Fatalf("order wrong: %v %v %v", got[0].DecisionID, got[1].DecisionID, got[2].DecisionID)
	}
}

// Ring buffer 满后写入覆盖最旧；Recent 仍按"最新→最旧"返回。
func TestMemSink_RingOverwrite(t *testing.T) {
	s := NewMemSink(3)
	ids := []string{"a", "b", "c", "d", "e"} // d/e 覆盖 a/b
	for _, id := range ids {
		s.Write(context.Background(), mkAudit(id))
	}
	got := s.Recent(10)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	want := []string{"e", "d", "c"}
	for i := range want {
		if got[i].DecisionID != want[i] {
			t.Fatalf("idx %d: want %s got %s", i, want[i], got[i].DecisionID)
		}
	}
}

func TestMemSink_LimitTruncates(t *testing.T) {
	s := NewMemSink(8)
	for i := 0; i < 5; i++ {
		s.Write(context.Background(), mkAudit(string(rune('a'+i))))
	}
	if got := s.Recent(2); len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
}

func TestLogSink_NilSafe(t *testing.T) {
	var s *LogSink
	s.Write(context.Background(), mkAudit("x")) // 不该 panic
}

func TestMultiSink_FansOut(t *testing.T) {
	mem1 := NewMemSink(2)
	mem2 := NewMemSink(2)
	multi := MultiSink{mem1, mem2}
	multi.Write(context.Background(), mkAudit("z"))
	if mem1.Recent(1)[0].DecisionID != "z" {
		t.Fatal("mem1 missed")
	}
	if mem2.Recent(1)[0].DecisionID != "z" {
		t.Fatal("mem2 missed")
	}
}

func TestLogSink_BasicWrite(t *testing.T) {
	s := &LogSink{Logger: zap.NewNop()}
	s.Write(context.Background(), mkAudit("y")) // 仅检查不 panic + 不阻塞
}

// ── Search 端点测试 ─────────────────────────────────────────

func mkSearchAudit(id, merchant, customer, ip, verdict string, when time.Time) *DecisionAudit {
	return &DecisionAudit{
		DecisionID: id,
		OccurredAt: when,
		Verdict:    verdict,
		RiskScore:  50,
		RiskLevel:  "med",
		Input: AuditInput{
			MerchantID: merchant,
			CustomerID: customer,
			IPAddress:  ip,
		},
	}
}

func TestMemSink_Search_FiltersByMerchant(t *testing.T) {
	s := NewMemSink(8)
	now := time.Now()
	s.Write(context.Background(), mkSearchAudit("d1", "m_a", "c1", "1.1.1.1", "DENY", now))
	s.Write(context.Background(), mkSearchAudit("d2", "m_b", "c2", "2.2.2.2", "ALLOW", now))
	s.Write(context.Background(), mkSearchAudit("d3", "m_a", "c3", "3.3.3.3", "REVIEW", now))

	got := s.Search(SearchFilter{MerchantID: "m_a"})
	if len(got) != 2 {
		t.Fatalf("expected 2 m_a; got %d", len(got))
	}
}

func TestMemSink_Search_FiltersByCustomerAndVerdict(t *testing.T) {
	s := NewMemSink(8)
	now := time.Now()
	s.Write(context.Background(), mkSearchAudit("d1", "m1", "c_x", "1.1.1.1", "DENY", now))
	s.Write(context.Background(), mkSearchAudit("d2", "m1", "c_x", "1.1.1.1", "ALLOW", now))
	s.Write(context.Background(), mkSearchAudit("d3", "m1", "c_y", "1.1.1.1", "DENY", now))

	got := s.Search(SearchFilter{CustomerID: "c_x", Verdict: "DENY"})
	if len(got) != 1 || got[0].DecisionID != "d1" {
		t.Fatalf("expected only d1; got %+v", got)
	}
}

func TestMemSink_Search_TimeRange(t *testing.T) {
	s := NewMemSink(8)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(1 * time.Hour)
	t2 := t0.Add(2 * time.Hour)
	s.Write(context.Background(), mkSearchAudit("d1", "m", "c", "ip", "ALLOW", t0))
	s.Write(context.Background(), mkSearchAudit("d2", "m", "c", "ip", "ALLOW", t1))
	s.Write(context.Background(), mkSearchAudit("d3", "m", "c", "ip", "ALLOW", t2))

	got := s.Search(SearchFilter{Since: t1, Until: t2})
	if len(got) != 2 {
		t.Fatalf("expected 2 in range; got %d", len(got))
	}
}

func TestMemSink_Search_LimitsResults(t *testing.T) {
	s := NewMemSink(20)
	now := time.Now()
	for i := 0; i < 10; i++ {
		s.Write(context.Background(), mkSearchAudit("d"+string(rune('0'+i)), "m", "c", "ip", "DENY", now))
	}
	got := s.Search(SearchFilter{Limit: 3})
	if len(got) != 3 {
		t.Fatalf("expected limit=3; got %d", len(got))
	}
}

func TestMemSink_Search_DescendingOrder(t *testing.T) {
	s := NewMemSink(8)
	s.Write(context.Background(), mkSearchAudit("a", "m", "c", "ip", "ALLOW", time.Unix(1, 0)))
	s.Write(context.Background(), mkSearchAudit("b", "m", "c", "ip", "ALLOW", time.Unix(2, 0)))
	s.Write(context.Background(), mkSearchAudit("c", "m", "c", "ip", "ALLOW", time.Unix(3, 0)))
	got := s.Search(SearchFilter{})
	if len(got) != 3 || got[0].DecisionID != "c" || got[2].DecisionID != "a" {
		t.Fatalf("expected newest-first; got %v %v %v",
			got[0].DecisionID, got[1].DecisionID, got[2].DecisionID)
	}
}

func TestMemSink_Search_EmptyFilterReturnsAll(t *testing.T) {
	s := NewMemSink(8)
	now := time.Now()
	for i := 0; i < 3; i++ {
		s.Write(context.Background(), mkSearchAudit("d"+string(rune('0'+i)), "m", "c", "ip", "ALLOW", now))
	}
	if got := s.Search(SearchFilter{}); len(got) != 3 {
		t.Fatalf("empty filter should return all 3; got %d", len(got))
	}
}

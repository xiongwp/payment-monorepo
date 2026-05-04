package merchantlist

import (
	"context"
	"testing"
	"time"
)

func TestMemService_AddContains(t *testing.T) {
	s := NewMemService()
	ctx := context.Background()
	if err := s.Add(ctx, Entry{
		MerchantID: "m1", Kind: KindBlock, Dimension: "customer", Value: "c1", Reason: "chargeback",
	}); err != nil {
		t.Fatal(err)
	}
	if e, ok := s.Contains(ctx, "m1", KindBlock, "customer", "c1"); !ok || e.Reason != "chargeback" {
		t.Fatalf("block hit failed: %+v ok=%v", e, ok)
	}
	if _, ok := s.Contains(ctx, "m1", KindAllow, "customer", "c1"); ok {
		t.Fatal("wrong kind should not match")
	}
	if _, ok := s.Contains(ctx, "m_other", KindBlock, "customer", "c1"); ok {
		t.Fatal("cross-merchant leak")
	}
}

func TestMemService_NormalizeCase(t *testing.T) {
	s := NewMemService()
	ctx := context.Background()
	_ = s.Add(ctx, Entry{MerchantID: "M1", Kind: KindAllow, Dimension: "EMAIL", Value: " VIP@Foo.com "})
	if _, ok := s.Contains(ctx, "m1", KindAllow, "email", "vip@foo.com"); !ok {
		t.Fatal("case / whitespace should normalize")
	}
}

func TestMemService_Expiry(t *testing.T) {
	s := NewMemService()
	ctx := context.Background()
	_ = s.Add(ctx, Entry{
		MerchantID: "m1", Kind: KindAllow, Dimension: "ip", Value: "1.2.3.4",
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	})
	if _, ok := s.Contains(ctx, "m1", KindAllow, "ip", "1.2.3.4"); ok {
		t.Fatal("expired entry should not match")
	}
	if got := s.List(ctx, "m1", KindAllow); len(got) != 0 {
		t.Fatalf("expired entry should not list, got %d", len(got))
	}
}

func TestMemService_RemoveAndList(t *testing.T) {
	s := NewMemService()
	ctx := context.Background()
	for _, v := range []string{"c1", "c2", "c3"} {
		_ = s.Add(ctx, Entry{MerchantID: "m1", Kind: KindBlock, Dimension: "customer", Value: v})
	}
	if got := s.List(ctx, "m1", KindBlock); len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	if !s.Remove(ctx, "m1", KindBlock, "customer", "c2") {
		t.Fatal("Remove should report true on hit")
	}
	if s.Remove(ctx, "m1", KindBlock, "customer", "c2") {
		t.Fatal("Remove should report false on miss")
	}
	if got := s.List(ctx, "m1", KindBlock); len(got) != 2 {
		t.Fatalf("after remove: expected 2, got %d", len(got))
	}
}

func TestMemService_AddValidation(t *testing.T) {
	s := NewMemService()
	ctx := context.Background()
	if err := s.Add(ctx, Entry{Kind: KindAllow, Dimension: "ip", Value: "1.2.3.4"}); err == nil {
		t.Fatal("missing merchant should fail")
	}
	if err := s.Add(ctx, Entry{MerchantID: "m1", Kind: "weird", Dimension: "ip", Value: "1"}); err == nil {
		t.Fatal("invalid kind should fail")
	}
	if err := s.Add(ctx, Entry{MerchantID: "m1", Kind: KindAllow, Dimension: "", Value: "1"}); err == nil {
		t.Fatal("empty dimension should fail")
	}
}

package webhook

import (
	"testing"
	"time"
)

func TestMemDLQ_PutGetList(t *testing.T) {
	s := NewMemDLQStore(0)
	_ = s.Put(DLQEntry{
		EventID: "e1", MerchantID: "m1", EventType: EventReviewCreated,
		Body: []byte(`{}`), Attempts: 5, LastError: "5xx",
	})
	got, ok := s.Get("e1")
	if !ok || got.EventID != "e1" {
		t.Fatalf("Get failed: %+v / %v", got, ok)
	}
	if got.Status != "pending" {
		t.Fatalf("default status should be 'pending'; got %q", got.Status)
	}
}

func TestMemDLQ_PutSameIDUpdates(t *testing.T) {
	s := NewMemDLQStore(0)
	first := time.Now().Add(-1 * time.Hour)
	_ = s.Put(DLQEntry{EventID: "e1", FirstFailedAt: first, Attempts: 1, LastError: "first"})
	_ = s.Put(DLQEntry{EventID: "e1", Attempts: 5, LastError: "fifth", LastAttemptAt: time.Now()})
	got, _ := s.Get("e1")
	if got.Attempts != 5 || got.LastError != "fifth" {
		t.Fatalf("update failed: %+v", got)
	}
	// FirstFailedAt 应该被保留
	if !got.FirstFailedAt.Equal(first) {
		t.Fatalf("FirstFailedAt should be preserved; got %v", got.FirstFailedAt)
	}
}

func TestMemDLQ_RingEvictsOldest(t *testing.T) {
	s := NewMemDLQStore(2)
	for _, id := range []string{"a", "b", "c"} {
		_ = s.Put(DLQEntry{EventID: id})
	}
	if _, ok := s.Get("a"); ok {
		t.Fatal("oldest 'a' should have been evicted")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("'c' should be retained")
	}
}

func TestMemDLQ_FilterByMerchantAndStatus(t *testing.T) {
	s := NewMemDLQStore(0)
	_ = s.Put(DLQEntry{EventID: "e1", MerchantID: "m1", Status: "pending"})
	_ = s.Put(DLQEntry{EventID: "e2", MerchantID: "m1", Status: "discarded"})
	_ = s.Put(DLQEntry{EventID: "e3", MerchantID: "m2", Status: "pending"})

	if got := s.List("m1", "", 10); len(got) != 2 {
		t.Fatalf("expected 2 m1; got %d", len(got))
	}
	if got := s.List("", "discarded", 10); len(got) != 1 || got[0].EventID != "e2" {
		t.Fatalf("expected only e2 discarded; got %+v", got)
	}
	if got := s.List("m1", "pending", 10); len(got) != 1 || got[0].EventID != "e1" {
		t.Fatalf("expected only e1; got %+v", got)
	}
}

func TestMemDLQ_DiscardAndDelete(t *testing.T) {
	s := NewMemDLQStore(0)
	_ = s.Put(DLQEntry{EventID: "e1", Status: "pending"})

	_ = s.Discard("e1", "alice", "merchant requested")
	got, _ := s.Get("e1")
	if got.Status != "discarded" {
		t.Fatalf("expected discarded; got %q", got.Status)
	}

	_ = s.Delete("e1")
	if _, ok := s.Get("e1"); ok {
		t.Fatal("Delete should remove")
	}
	if s.Size() != 0 {
		t.Fatalf("size should be 0; got %d", s.Size())
	}
}

func TestMemDLQ_NilSafe(t *testing.T) {
	var s *MemDLQStore
	_ = s.Put(DLQEntry{EventID: "x"})
	if _, ok := s.Get("x"); ok {
		t.Fatal("nil store get should miss")
	}
	if s.Size() != 0 {
		t.Fatal("nil size should be 0")
	}
	_ = s.Discard("x", "a", "r")
	_ = s.Delete("x")
}

func TestMemDLQ_ListLimit(t *testing.T) {
	s := NewMemDLQStore(0)
	for i := 0; i < 50; i++ {
		_ = s.Put(DLQEntry{EventID: string(rune('a' + i%26))})
	}
	if got := s.List("", "", 10); len(got) != 10 {
		t.Fatalf("expected limit=10; got %d", len(got))
	}
}

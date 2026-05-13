package store

import (
	"context"
	"testing"
	"time"
)

func mkEvt(svc, table, pk string, idx map[string]string) Event {
	return Event{
		Service:   svc,
		Table:     table,
		PK:        pk,
		Op:        "insert",
		Timestamp: time.Now(),
		Indexes:   idx,
	}
}

func TestFixture_Empty(t *testing.T) {
	fs := NewFixtureSearcher(nil)
	if fs.Count() != 0 {
		t.Fatal("empty count not 0")
	}
	got, _ := fs.SearchByIndex(context.Background(), "any", "x")
	if got != nil {
		t.Fatal("empty SearchByIndex should return nil")
	}
}

func TestFixture_GetEvent(t *testing.T) {
	fs := NewFixtureSearcher([]Event{
		mkEvt("oc", "pi", "pi_1", map[string]string{"pi_id": "pi_1"}),
	})
	e, _ := fs.GetEvent(context.Background(), "oc", "pi", "pi_1")
	if e == nil || e.PK != "pi_1" {
		t.Errorf("get failed: %+v", e)
	}
	miss, _ := fs.GetEvent(context.Background(), "oc", "pi", "missing")
	if miss != nil {
		t.Errorf("miss should be nil")
	}
}

func TestFixture_SearchByIndex(t *testing.T) {
	fs := NewFixtureSearcher([]Event{
		mkEvt("oc", "pi", "pi_1", map[string]string{"pi_id": "pi_1"}),
		mkEvt("pc", "tx", "tx_a", map[string]string{"pi_id": "pi_1"}),
		mkEvt("pc", "tx", "tx_b", map[string]string{"pi_id": "pi_2"}),
	})
	hits, _ := fs.SearchByIndex(context.Background(), "pi_id", "pi_1")
	if len(hits) != 2 {
		t.Errorf("want 2 hits for pi_1, got %d", len(hits))
	}
}

func TestFixture_ScanService(t *testing.T) {
	fs := NewFixtureSearcher([]Event{
		mkEvt("oc", "pi", "pi_1", nil),
		mkEvt("oc", "pi", "pi_2", nil),
		mkEvt("oc", "order", "o_1", nil),
	})
	out, _ := fs.ScanService(context.Background(), "oc", "pi", 100)
	if len(out) != 2 {
		t.Errorf("want 2 pi rows, got %d", len(out))
	}
}

func TestFixture_ScanIndex(t *testing.T) {
	fs := NewFixtureSearcher([]Event{
		mkEvt("oc", "pi", "pi_1", map[string]string{"pi_id": "pi_apple"}),
		mkEvt("oc", "pi", "pi_2", map[string]string{"pi_id": "pi_banana"}),
		mkEvt("oc", "pi", "pi_3", map[string]string{"pi_id": "ap_x"}),
	})
	all, _ := fs.ScanIndex(context.Background(), "pi_id", "", 100)
	if len(all) != 3 {
		t.Errorf("want 3 distinct values, got %d", len(all))
	}
	pi, _ := fs.ScanIndex(context.Background(), "pi_id", "pi_", 100)
	if len(pi) != 2 {
		t.Errorf("want 2 pi_-prefixed, got %d (%v)", len(pi), pi)
	}
	limit, _ := fs.ScanIndex(context.Background(), "pi_id", "", 1)
	if len(limit) != 1 {
		t.Errorf("limit not honored")
	}
}

func TestFixture_AddIncremental(t *testing.T) {
	fs := NewFixtureSearcher(nil)
	fs.Add(mkEvt("oc", "pi", "pi_1", map[string]string{"pi_id": "pi_1"}))
	if fs.Count() != 1 {
		t.Errorf("count=%d after Add", fs.Count())
	}
	e, _ := fs.GetEvent(context.Background(), "oc", "pi", "pi_1")
	if e == nil {
		t.Error("added event not found")
	}
}

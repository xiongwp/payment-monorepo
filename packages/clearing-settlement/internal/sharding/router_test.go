package sharding

import "testing"

func TestRouter_DefaultDimensions(t *testing.T) {
	r := NewRouter()
	if r.DBCount() != 10 || r.TablePerDB() != 10 {
		t.Fatalf("expected 10x10, got %dx%d", r.DBCount(), r.TablePerDB())
	}
	if got := len(r.AllShards()); got != 100 {
		t.Fatalf("expected 100 shards, got %d", got)
	}
}

func TestRouter_StableRouting(t *testing.T) {
	r := NewRouter()
	// 同 merchant_id 必须每次落同一分片
	d1, t1 := r.RouteByMerchantID("MCH_001")
	for i := 0; i < 10; i++ {
		d, tbl := r.RouteByMerchantID("MCH_001")
		if d != d1 || tbl != t1 {
			t.Fatalf("non-stable routing: got (%d,%d), expected (%d,%d)", d, tbl, d1, t1)
		}
	}
}

func TestRouter_RouteWithinBounds(t *testing.T) {
	r := NewRouter()
	for i := 0; i < 1000; i++ {
		d, tbl := r.RouteByMerchantID(string(rune('A'+i%26)) + "MCH_TEST")
		if d < 0 || d >= 10 {
			t.Fatalf("dbIndex out of range: %d", d)
		}
		if tbl < 0 || tbl >= 10 {
			t.Fatalf("tableIndex out of range: %d", tbl)
		}
	}
}

func TestRouter_TableNames(t *testing.T) {
	r := NewRouter()
	if got := r.RunTableName(7); got != "settlement_run_07" {
		t.Fatalf("expected settlement_run_07, got %s", got)
	}
	if got := r.RecordTableName(42); got != "settlement_record_42" {
		t.Fatalf("expected settlement_record_42, got %s", got)
	}
}

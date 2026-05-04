package sharding

import "testing"

func TestRouter_RouteByID(t *testing.T) {
	r := NewRouter()
	db, tbl := r.RouteByID(123)
	if db < 0 || db >= 10 || tbl < 0 || tbl >= 100 || tbl/10 != db {
		t.Fatalf("bad shard: %d %d", db, tbl)
	}
}

func TestRouter_RouteByPrefixedID(t *testing.T) {
	r := NewRouter()
	// pi_4471234560001 → db=4, tbl=47（tbl/10 == db 的有效组合）
	db, tbl := r.RouteByPrefixedID("pi_4471234560001")
	if db != 4 || tbl != 47 {
		t.Fatalf("want db=4 tbl=47 got %d %d", db, tbl)
	}
	// 不合法组合（db=4, tbl=37）时降级到 tbl/10 算出的分片
	db2, tbl2 := r.RouteByPrefixedID("pi_4371234560001")
	if db2 != 3 || tbl2 != 37 {
		t.Fatalf("expected fallback db=3 tbl=37, got %d %d", db2, tbl2)
	}
	// 无前缀时按字符串哈希
	db3, tbl3 := r.RouteByPrefixedID("abc")
	if db3 < 0 || tbl3 < 0 {
		t.Fatalf("hash shard bad: %d %d", db3, tbl3)
	}
}

func TestRouter_FormatID(t *testing.T) {
	r := NewRouter()
	id := r.FormatID("aq", 3, 37, 12345)
	if id != "aq_33712345" {
		t.Fatalf("want aq_33712345 got %s", id)
	}
}

func TestRouter_GetTableName(t *testing.T) {
	r := NewRouter()
	if got := r.GetTableName("acquirer_tx", 5); got != "acquirer_tx_05" {
		t.Fatalf("want acquirer_tx_05 got %s", got)
	}
	if got := r.GetTableName("acquirer_tx", 99); got != "acquirer_tx_99" {
		t.Fatalf("want acquirer_tx_99 got %s", got)
	}
}

func TestRouter_AllShards(t *testing.T) {
	r := NewRouter()
	all := r.AllShards()
	if len(all) != 100 {
		t.Fatalf("want 100 shards got %d", len(all))
	}
}

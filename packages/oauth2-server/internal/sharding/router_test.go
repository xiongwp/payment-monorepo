package sharding

import (
	"strings"
	"testing"
)

func TestRouteByKey_Deterministic(t *testing.T) {
	r := NewRouter()
	d1, t1 := r.RouteByKey("mer_001")
	d2, t2 := r.RouteByKey("mer_001")
	if d1 != d2 || t1 != t2 {
		t.Errorf("not deterministic: (%d,%d) vs (%d,%d)", d1, t1, d2, t2)
	}
}

func TestRouteByKey_Spread(t *testing.T) {
	r := NewRouter()
	seen := map[int]int{}
	for i := 0; i < 10000; i++ {
		_, tbl := r.RouteByKey("client_" + string(rune('a'+(i%26))) + intStr(i))
		seen[tbl]++
	}
	// 每个 shard 至少有 1 个落入 (10000 个 key 落 100 个 shard, 均匀的话应该 ~100/shard)
	if len(seen) < 90 {
		t.Errorf("expected ≥90 shards used, got %d", len(seen))
	}
}

func TestClientsTable(t *testing.T) {
	r := NewRouter()
	db, tbl := r.ClientsTable("mer_merchant_001_abc")
	if !strings.HasPrefix(db, "oauth2_db_") {
		t.Errorf("bad db: %s", db)
	}
	if !strings.HasPrefix(tbl, "clients_") {
		t.Errorf("bad table: %s", tbl)
	}
}

func TestAllShards_Complete(t *testing.T) {
	r := NewRouter()
	shards := r.AllShards("clients")
	if len(shards) != 100 {
		t.Errorf("want 100 shards, got %d", len(shards))
	}
	// 第 1 个和最后 1 个的格式
	if shards[0].Table != "clients_00" {
		t.Errorf("first table=%s", shards[0].Table)
	}
	if shards[99].Table != "clients_99" {
		t.Errorf("last table=%s", shards[99].Table)
	}
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	out := []byte{}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

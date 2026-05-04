package sharding

import (
	"testing"
)

func TestRouteByID(t *testing.T) {
	r := NewRouterWithConfig(10, 10)
	// 100 表循环
	seen := make(map[int]bool)
	for id := int64(0); id < 200; id++ {
		_, tbl := r.RouteByID(id)
		if tbl < 0 || tbl >= 100 {
			t.Fatalf("tbl %d out of range", tbl)
		}
		seen[tbl] = true
	}
	if len(seen) != 100 {
		t.Fatalf("expected all 100 tables reached, got %d", len(seen))
	}
}

func TestRouteByPrefixedIDStable(t *testing.T) {
	r := NewRouterWithConfig(10, 10)
	id := r.FormatID("pi", 3, 37, 123456789)
	if id[:3] != "pi_" {
		t.Fatalf("prefix missing: %s", id)
	}
	db, tbl := r.RouteByPrefixedID(id)
	if db != 3 || tbl != 37 {
		t.Fatalf("expected db=3 tbl=37, got db=%d tbl=%d (id=%s)", db, tbl, id)
	}
}

func TestRouteByStringConsistent(t *testing.T) {
	r := NewRouterWithConfig(10, 10)
	mchA := "mch_shop_abc"
	db1, tbl1 := r.RouteByString(mchA)
	db2, tbl2 := r.RouteByString(mchA)
	if db1 != db2 || tbl1 != tbl2 {
		t.Fatalf("inconsistent routing: (%d,%d) vs (%d,%d)", db1, tbl1, db2, tbl2)
	}
}

func TestRouteByPrefixedIDEqualsRouteByString(t *testing.T) {
	// 核心不变量：同 business_id 生成的 pi_id 路由结果必须等于 business_id 自身路由
	r := NewRouterWithConfig(10, 10)
	biz := "merchant_shop_42"
	db, tbl := r.RouteByString(biz)
	pi := r.FormatID("pi", db, tbl, 99999)
	db2, tbl2 := r.RouteByPrefixedID(pi)
	if db2 != db || tbl2 != tbl {
		t.Fatalf("pi_id routing diverged from business_id: biz=(%d,%d) pi=(%d,%d)",
			db, tbl, db2, tbl2)
	}
}

func TestGetTableName(t *testing.T) {
	r := NewRouterWithConfig(10, 10)
	if got := r.GetTableName("payment_intent", 7); got != "payment_intent_07" {
		t.Fatalf("expected payment_intent_07, got %s", got)
	}
	if got := r.GetTableName("charge", 99); got != "charge_99" {
		t.Fatalf("expected charge_99, got %s", got)
	}
}

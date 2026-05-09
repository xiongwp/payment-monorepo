package sharding

import (
	"context"
	"testing"
)

func TestRouterV2_Defaults(t *testing.T) {
	r := NewRouterV2()
	if r.DBCount() != ShardDBCountV2 || r.TablePerDB() != ShardTablePerDBV2 {
		t.Fatalf("defaults wrong: %d × %d", r.DBCount(), r.TablePerDB())
	}
	if r.TotalTableCount() != ShardDBCountV2*ShardTablePerDBV2 {
		t.Fatalf("total wrong: %d", r.TotalTableCount())
	}
}

func TestRouterV2_NewWithConfig_Clamps(t *testing.T) {
	r := NewRouterV2WithConfig(0, 0)
	if r.DBCount() != ShardDBCountV2 || r.TablePerDB() != ShardTablePerDBV2 {
		t.Fatalf("zero clamp wrong")
	}
	r = NewRouterV2WithConfig(100_000, 100_000)
	if r.DBCount() != ShardDBCountV2 || r.TablePerDB() != ShardTablePerDBV2 {
		t.Fatalf("excess clamp wrong")
	}
}

func TestRouterV2_RouteByID_Cycle(t *testing.T) {
	r := NewRouterV2WithConfig(2, 5) // 10 shard
	for id := int64(0); id < 30; id++ {
		db, tbl := r.RouteByID(id)
		if db < 0 || db >= 2 || tbl < 0 || tbl >= 10 {
			t.Errorf("id=%d → (%d,%d) OOR", id, db, tbl)
		}
	}
}

func TestRouterV2_RouteByPIID_Stable(t *testing.T) {
	r := NewRouterV2()
	a, b := r.RouteByPIID("pi_xyz_42")
	c, d := r.RouteByPIID("pi_xyz_42")
	if a != c || b != d {
		t.Fatalf("non-deterministic: (%d,%d) vs (%d,%d)", a, b, c, d)
	}
}

func TestRouterV2_TableName_V2Layout(t *testing.T) {
	r := NewRouterV2()
	got := r.TableName(context.Background(), "card_transaction", 7, 813)
	want := "card_transaction_07_813"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestRouterV2_AllShards_Cardinality(t *testing.T) {
	r := NewRouterV2WithConfig(2, 3)
	if got := len(r.AllShards()); got != 6 {
		t.Fatalf("expected 6 shards, got %d", got)
	}
}

func TestSetCurrentLayoutVersion_RejectsUnknown(t *testing.T) {
	old := CurrentLayoutVersion
	defer func() { CurrentLayoutVersion = old }()
	CurrentLayoutVersion = LayoutV1
	SetCurrentLayoutVersion(LayoutVersion(99))
	if CurrentLayoutVersion != LayoutV1 {
		t.Fatalf("unknown version should be ignored")
	}
}

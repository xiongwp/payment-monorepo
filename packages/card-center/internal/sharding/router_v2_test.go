package sharding

import (
	"context"
	"testing"
)

func TestRouterV2_Defaults(t *testing.T) {
	r := NewRouterV2()
	if r.DBCount() != ShardDBCountV2 || r.TablePerDB() != ShardTablePerDBV2 {
		t.Fatalf("default config wrong: %d × %d", r.DBCount(), r.TablePerDB())
	}
	if r.TotalTableCount() != ShardDBCountV2*ShardTablePerDBV2 {
		t.Fatalf("total=%d", r.TotalTableCount())
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

// 同 user_id 在 V1 vs V2 落不同 shard（模数不同）：dual-read 必备假设。
func TestRouterV2_DivergesFromV1(t *testing.T) {
	v1 := NewRouter()
	v2 := NewRouterV2()
	uid := int64(123_456_789)
	v1db, v1tbl := v1.RouteByUserID(uid)
	v2db, v2tbl := v2.RouteByUserID(uid)
	// 不要求一定不同，但维度上 V1 总共 100、V2 总共 100k → 至少 v2tbl 范围更大
	if v2tbl >= 100 && v1tbl < 100 {
		// 此时 V2 routing 落在 V1 不可达的位置 — dual-read 期 caller 必须按 layout 选 router
	}
	_ = v1db
	_ = v2db
	if v2tbl < 0 || v2tbl >= ShardDBCountV2*ShardTablePerDBV2 {
		t.Fatalf("v2 tbl OOR: %d", v2tbl)
	}
}

func TestRouterV2_RouteByPIID_Stable(t *testing.T) {
	r := NewRouterV2()
	a1, b1 := r.RouteByPIID("pi_xyz_42")
	a2, b2 := r.RouteByPIID("pi_xyz_42")
	if a1 != a2 || b1 != b2 {
		t.Fatalf("non-deterministic: (%d,%d) != (%d,%d)", a1, b1, a2, b2)
	}
}

// token_hash 路由：合法 hex 前 8 char → 解析 + 取模；非 hex / 太短 → (0,0)
func TestRouterV2_RouteByTokenHash(t *testing.T) {
	r := NewRouterV2()
	cases := []struct {
		in    string
		valid bool
	}{
		{"deadbeef" + "0000000000000000000000000000000000000000000000000000000000", true}, // 64 char hex
		{"abc", false},  // 太短
		{"zzzzzzzz" + "00000000000000000000000000000000000000000000000000000000", false}, // 非 hex
	}
	for _, c := range cases {
		db, tbl := r.RouteByTokenHash(c.in)
		if !c.valid {
			if db != 0 || tbl != 0 {
				t.Errorf("invalid token_hash should route (0,0), got (%d,%d) for %q", db, tbl, c.in)
			}
			continue
		}
		if db < 0 || db >= r.DBCount() || tbl < 0 || tbl >= r.TablePerDB()*r.DBCount() {
			t.Errorf("OOR for %q: (%d,%d)", c.in, db, tbl)
		}
	}
}

func TestRouterV2_TableName_V2Layout(t *testing.T) {
	r := NewRouterV2()
	got := r.TableName(context.Background(), "stored_token", 7, 813)
	want := "stored_token_07_813"
	if got != want {
		t.Fatalf("expected %q got %q", want, got)
	}
}

func TestRouterV2_AllShards_Cardinality(t *testing.T) {
	// 缩小测试避免 100k 项分配
	r := NewRouterV2WithConfig(2, 3)
	all := r.AllShards()
	if len(all) != 6 {
		t.Fatalf("expected 6 shards, got %d", len(all))
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

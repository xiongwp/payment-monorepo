package sharding

import (
	"strings"
	"testing"
)

func TestRouterV2_Defaults(t *testing.T) {
	r := NewRouterV2()
	if r.DBCount() != DefaultShardDBCountV2 || r.TablePerDB() != DefaultTablePerDBV2 {
		t.Fatalf("defaults wrong: %d × %d", r.DBCount(), r.TablePerDB())
	}
	if r.TotalTableCount() != DefaultShardDBCountV2*DefaultTablePerDBV2 {
		t.Fatalf("total wrong: %d", r.TotalTableCount())
	}
}

func TestRouterV2_NewWithConfig_Clamps(t *testing.T) {
	r := NewRouterV2WithConfig(0, 0)
	if r.DBCount() != DefaultShardDBCountV2 || r.TablePerDB() != DefaultTablePerDBV2 {
		t.Fatalf("zero clamp wrong")
	}
	r = NewRouterV2WithConfig(100_000, 100_000)
	if r.DBCount() != DefaultShardDBCountV2 || r.TablePerDB() != DefaultTablePerDBV2 {
		t.Fatalf("excess clamp wrong")
	}
}

func TestRouterV2_FormatAndRoute(t *testing.T) {
	r := NewRouterV2()
	id := r.FormatIDV2("ch", 7, 813, 12345)
	if !strings.HasPrefix(id, "ch_v2_") {
		t.Fatalf("expected v2 prefix, got %q", id)
	}
	db, tbl := r.RouteByPrefixedID(id)
	if db != 7 || tbl != 813 {
		t.Fatalf("expected db=7 tbl=813 got %d/%d (id=%s)", db, tbl, id)
	}
}

func TestLayoutVersionOfID(t *testing.T) {
	cases := []struct {
		in   string
		want LayoutVersion
	}{
		{"ch_337999", LayoutV1},
		{"ch_v2_03037_99999", LayoutV2},
		{"pi_v2_07042_111", LayoutV2},
		{"pi_337", LayoutV1},
		{"", LayoutV1},
		{"ch_v3_unknown", LayoutV1},
	}
	for _, c := range cases {
		if got := LayoutVersionOfID(c.in); got != c.want {
			t.Errorf("LayoutVersionOfID(%q)=%v, want %v", c.in, got, c.want)
		}
	}
}

// V1 ID 在 V2.RouteByPrefixedID 走 fallback (RouteByString) — 不 panic，结果有界。
func TestRouterV2_FallbackOnV1String(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2 := NewRouterV2()
	id := v1.FormatID("ch", 3, 37, 99999)
	db, tbl := v2.RouteByPrefixedID(id)
	if db < 0 || db >= v2.DBCount() || tbl < 0 || tbl >= v2.TablePerDB()*v2.DBCount() {
		t.Fatalf("V2 fallback OOR: %d/%d", db, tbl)
	}
}

// dispatcher V1 ID → V1 router; V2 ID → V2 router；正确报告 layout version。
func TestRouteByPrefixedIDDual(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2 := NewRouterV2()
	v1id := v1.FormatID("ch", 4, 47, 1001)
	v2id := v2.FormatIDV2("ch", 4, 477, 1001)

	if db, tbl, ver := RouteByPrefixedIDDual(v1, v2, v1id); db != 4 || tbl != 47 || ver != LayoutV1 {
		t.Fatalf("V1 dispatch wrong: %d/%d ver=%d", db, tbl, ver)
	}
	if db, tbl, ver := RouteByPrefixedIDDual(v1, v2, v2id); db != 4 || tbl != 477 || ver != LayoutV2 {
		t.Fatalf("V2 dispatch wrong: %d/%d ver=%d", db, tbl, ver)
	}
}

// dispatcher 兼容 v2 router 缺失（dual-read 未启用）：V2 ID 仍要 routable。
func TestRouteByPrefixedIDDual_NilV2_Fallback(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	db, tbl, ver := RouteByPrefixedIDDual(v1, nil, "ch_v2_05123_42")
	if ver != LayoutV2 {
		t.Fatalf("nil V2 should still report layout=V2, got %d", ver)
	}
	if db < 0 || db >= v1.DBCount() {
		t.Fatalf("V1 fallback OOR db: %d", db)
	}
	if tbl < 0 || tbl >= v1.TablePerDB()*v1.DBCount() {
		t.Fatalf("V1 fallback OOR tbl: %d", tbl)
	}
}

func TestFormatIDForCurrentLayout_Switching(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2 := NewRouterV2()
	old := CurrentLayoutVersion
	defer SetCurrentLayoutVersion(old)

	SetCurrentLayoutVersion(LayoutV1)
	if id := FormatIDForCurrentLayout(v1, v2, "ch", 3, 37, 1); LayoutVersionOfID(id) != LayoutV1 {
		t.Fatalf("expected V1 id, got %q", id)
	}
	SetCurrentLayoutVersion(LayoutV2)
	if id := FormatIDForCurrentLayout(v1, v2, "ch", 3, 337, 1); LayoutVersionOfID(id) != LayoutV2 {
		t.Fatalf("expected V2 id, got %q", id)
	}
	if id := FormatIDForCurrentLayout(v1, nil, "ch", 3, 37, 1); LayoutVersionOfID(id) != LayoutV1 {
		t.Fatalf("nil v2 with LayoutV2 should fallback to V1 id, got %q", id)
	}
}

func TestRouterV2_StableRoundTrip(t *testing.T) {
	r := NewRouterV2()
	cases := []struct {
		db, tbl int
		seq     int64
	}{
		{0, 0, 1},
		{99, 999, 100_000_000},
		{50, 500, 12345},
	}
	for _, c := range cases {
		id := r.FormatIDV2("ch", c.db, c.tbl, c.seq)
		gotDB, gotTbl := r.RouteByPrefixedID(id)
		if gotDB != c.db || gotTbl != c.tbl {
			t.Errorf("(%d,%d) → %s → (%d,%d)", c.db, c.tbl, id, gotDB, gotTbl)
		}
	}
}

func TestRouterV2_TamperedIDFallback(t *testing.T) {
	r := NewRouterV2()
	bad := "ch_v2_AAxxx_42"
	db, tbl := r.RouteByPrefixedID(bad)
	if db < 0 || db >= r.DBCount() || tbl < 0 || tbl >= r.TablePerDB()*r.DBCount() {
		t.Fatalf("tampered id OOR: %d/%d", db, tbl)
	}
}

// RouteByIDHashed 对 bit-encoded ID 抗聚集 — 同一 id 多次结果稳定。
func TestRouterV2_RouteByIDHashed_Stable(t *testing.T) {
	r := NewRouterV2()
	id := int64(1234567890)
	a, b := r.RouteByIDHashed(id)
	c, d := r.RouteByIDHashed(id)
	if a != c || b != d {
		t.Fatalf("RouteByIDHashed unstable: (%d,%d) vs (%d,%d)", a, b, c, d)
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

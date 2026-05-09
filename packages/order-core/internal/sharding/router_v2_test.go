package sharding

import (
	"strings"
	"testing"
)

// V2 default config: 100 db × 1000 tbl = 100k shard.
func TestRouterV2_Defaults(t *testing.T) {
	r := NewRouterV2()
	if r.DBCount() != DefaultShardDBCountV2 {
		t.Fatalf("dbCount=%d", r.DBCount())
	}
	if r.TablePerDB() != DefaultTablePerDBV2 {
		t.Fatalf("tablePerDB=%d", r.TablePerDB())
	}
	if r.TotalTableCount() != DefaultShardDBCountV2*DefaultTablePerDBV2 {
		t.Fatalf("total=%d", r.TotalTableCount())
	}
}

// 越界 dbCount/tablePerDB 自动回退默认。
func TestRouterV2_NewWithConfig_ClampsOutOfRange(t *testing.T) {
	r := NewRouterV2WithConfig(0, 0)
	if r.DBCount() != DefaultShardDBCountV2 || r.TablePerDB() != DefaultTablePerDBV2 {
		t.Fatalf("zero clamp failed: %d / %d", r.DBCount(), r.TablePerDB())
	}
	r = NewRouterV2WithConfig(100_000, 100_000)
	// 超界 → 回退默认（避免编码溢出）
	if r.DBCount() != DefaultShardDBCountV2 || r.TablePerDB() != DefaultTablePerDBV2 {
		t.Fatalf("excess clamp failed: %d / %d", r.DBCount(), r.TablePerDB())
	}
}

// FormatIDV2 / RouteByPrefixedID 来回。
func TestRouterV2_FormatAndRoute(t *testing.T) {
	r := NewRouterV2()
	id := r.FormatIDV2("pi", 7, 813, 12345)
	if !strings.HasPrefix(id, "pi_v2_") {
		t.Fatalf("expected v2 prefix, got %q", id)
	}
	db, tbl := r.RouteByPrefixedID(id)
	if db != 7 || tbl != 813 {
		t.Fatalf("expected db=7 tbl=813 got %d/%d (id=%s)", db, tbl, id)
	}
}

// V2 不能识别 V1 字符串 → 回退 RouteByString。
func TestRouterV2_FallbackOnV1String(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2 := NewRouterV2()
	id := v1.FormatID("pi", 3, 37, 99999) // V1 layout
	if LayoutVersionOfID(id) != LayoutV1 {
		t.Fatalf("V1 layout misdetected: %q", id)
	}
	// V2.RouteByPrefixedID(v1id) 走 RouteByString（确定但非 v1 (3,37)）
	db, tbl := v2.RouteByPrefixedID(id)
	if db < 0 || db >= v2.DBCount() || tbl < 0 || tbl >= v2.TablePerDB() {
		t.Fatalf("v2 fallback OOR: %d/%d", db, tbl)
	}
}

// LayoutVersionOfID 探测：V1 默认 1，"_v2_" 标记 → 2。
func TestLayoutVersionOfID(t *testing.T) {
	cases := []struct {
		in   string
		want LayoutVersion
	}{
		{"pi_337999", LayoutV1},
		{"pi_v2_03037_99999", LayoutV2},
		{"ch_v2_07042_111", LayoutV2},
		{"re_007", LayoutV1},
		{"", LayoutV1},
		{"pi_", LayoutV1},
		{"pi_v3_doesnt_exist", LayoutV1}, // 未来版本不识别 → 当 V1
	}
	for _, c := range cases {
		got := LayoutVersionOfID(c.in)
		if got != c.want {
			t.Errorf("LayoutVersionOfID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// dispatcher：V1 ID → V1 router，V2 ID → V2 router；返回正确 version 标记。
func TestRouteByPrefixedIDDual(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2 := NewRouterV2()

	v1id := v1.FormatID("pi", 4, 47, 1001)
	v2id := v2.FormatIDV2("pi", 4, 477, 1001)

	if db, tbl, ver := RouteByPrefixedIDDual(v1, v2, v1id); db != 4 || tbl != 47 || ver != LayoutV1 {
		t.Fatalf("V1 dispatch wrong: %d/%d ver=%d (id=%s)", db, tbl, ver, v1id)
	}
	if db, tbl, ver := RouteByPrefixedIDDual(v1, v2, v2id); db != 4 || tbl != 477 || ver != LayoutV2 {
		t.Fatalf("V2 dispatch wrong: %d/%d ver=%d (id=%s)", db, tbl, ver, v2id)
	}
}

// dispatcher 兼容 v2 router 缺失（dual-read 未启用）：V2 ID 仍要被路由（兜底 hash），
// version 仍报 V2（caller 可以拿到这个信号，发警告 / metric）。
func TestRouteByPrefixedIDDual_NilV2_Fallback(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2id := "pi_v2_05123_42"
	db, tbl, ver := RouteByPrefixedIDDual(v1, nil, v2id)
	if ver != LayoutV2 {
		t.Fatalf("nil V2 should still report layout=V2, got %d", ver)
	}
	if db < 0 || db >= v1.DBCount() || tbl < 0 || tbl >= v1.TablePerDB()*v1.DBCount() {
		t.Fatalf("V1 fallback OOR: %d/%d", db, tbl)
	}
}

// FormatIDForCurrentLayout 跟全局开关联动。
func TestFormatIDForCurrentLayout_Switching(t *testing.T) {
	v1 := NewRouterWithConfig(10, 10)
	v2 := NewRouterV2()

	// 默认 V1
	old := CurrentLayoutVersion
	defer SetCurrentLayoutVersion(old)

	SetCurrentLayoutVersion(LayoutV1)
	if id := FormatIDForCurrentLayout(v1, v2, "pi", 3, 37, 1); LayoutVersionOfID(id) != LayoutV1 {
		t.Fatalf("expected V1 id under LayoutV1, got %q", id)
	}

	SetCurrentLayoutVersion(LayoutV2)
	if id := FormatIDForCurrentLayout(v1, v2, "pi", 3, 337, 1); LayoutVersionOfID(id) != LayoutV2 {
		t.Fatalf("expected V2 id under LayoutV2, got %q", id)
	}

	// V2 配置但 v2 router == nil → fallback V1，不该 panic
	if id := FormatIDForCurrentLayout(v1, nil, "pi", 3, 37, 1); LayoutVersionOfID(id) != LayoutV1 {
		t.Fatalf("nil v2 with LayoutV2 should fallback to V1 id, got %q", id)
	}
}

// SetCurrentLayoutVersion 拒绝未知版本（保护误用）。
func TestSetCurrentLayoutVersion_RejectsUnknown(t *testing.T) {
	old := CurrentLayoutVersion
	defer func() { CurrentLayoutVersion = old }()
	CurrentLayoutVersion = LayoutV1

	SetCurrentLayoutVersion(LayoutVersion(99))
	if CurrentLayoutVersion != LayoutV1 {
		t.Fatalf("unknown version should be ignored, got %d", CurrentLayoutVersion)
	}
}

// V1/V2 路由结果稳定 — 同 prefix+(db,tbl,seq) 重新 Format/Route 必须回到原值。
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
			t.Errorf("round-trip mismatch (%d,%d) -> %s -> (%d,%d)",
				c.db, c.tbl, id, gotDB, gotTbl)
		}
	}
}

// V2 篡改 ID（手动改字符）→ 不 panic，回落 RouteByString。
func TestRouterV2_TamperedIDFallback(t *testing.T) {
	r := NewRouterV2()
	bad := "pi_v2_AAxxx_42" // 非数字 db/tbl
	db, tbl := r.RouteByPrefixedID(bad)
	if db < 0 || db >= r.DBCount() || tbl < 0 || tbl >= r.TablePerDB() {
		t.Fatalf("tampered id OOR: %d/%d", db, tbl)
	}
}

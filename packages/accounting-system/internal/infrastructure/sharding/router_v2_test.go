package sharding

import (
	"context"
	"strings"
	"testing"
)

func TestRouterV2_Defaults(t *testing.T) {
	r := NewRouterV2()
	if r.DBCount() != ShardDBCountV2 || r.TablePerDB() != ShardTablePerDBV2 {
		t.Fatalf("default config wrong: %d × %d", r.DBCount(), r.TablePerDB())
	}
	if r.TotalTableCount() != ShardDBCountV2*ShardTablePerDBV2 {
		t.Fatalf("total wrong: %d", r.TotalTableCount())
	}
	if r.Version() != RouterV2Const {
		t.Fatalf("version=%d, want RouterV2Const", r.Version())
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

func TestRouterV2_RouteByID(t *testing.T) {
	r := NewRouterV2WithConfig(2, 5) // 10 shard 总数
	for id := int64(0); id < 100; id++ {
		db, tbl := r.RouteByID(id)
		if db < 0 || db >= 2 || tbl < 0 || tbl >= 10 {
			t.Errorf("id=%d → (%d,%d) OOR", id, db, tbl)
		}
		// dbIdx 与 globalTblIdx 一致：tbl/tablePerDB == db
		if tbl/r.tablePerDB != db {
			t.Errorf("id=%d: dbIdx %d != tbl/tablePerDB %d", id, db, tbl/r.tablePerDB)
		}
	}
}

func TestRouterV2_RouteByNumericStr(t *testing.T) {
	r := NewRouterV2()
	// 合法
	db1, tbl1 := r.RouteByNumericStr("12345")
	db2, tbl2 := r.RouteByID(12345)
	if db1 != db2 || tbl1 != tbl2 {
		t.Fatalf("RouteByNumericStr inconsistent with RouteByID")
	}
	// 非法
	db, tbl := r.RouteByNumericStr("not-a-number")
	if db != 0 || tbl != 0 {
		t.Fatalf("invalid string should route (0,0), got (%d,%d)", db, tbl)
	}
}

// account_no V2 路径必须返回非法 (-1, -1)，绝不能静默返 (0, 0)。
// EncodeAccountIDV2 落地之前调用方必须 panic / fail，避免数据散错位。
func TestRouterV2_RouteByAccountNo_RefusesUntilLayoutV2(t *testing.T) {
	r := NewRouterV2()
	db, tbl := r.RouteByAccountNo("1234567890123456789")
	if db != -1 || tbl != -1 {
		t.Fatalf("RouteByAccountNo must refuse with (-1,-1) until EncodeAccountIDV2 lands; got (%d,%d)", db, tbl)
	}
}

func TestRouterV2_TableName_V2Layout(t *testing.T) {
	r := NewRouterV2()
	got := r.TableName(context.Background(), "voucher", 7, 813)
	want := "voucher_07_813"
	if got != want {
		t.Fatalf("expected %q got %q", want, got)
	}
	if got2 := r.GetTableName("voucher", 7, 813); got2 != want {
		t.Fatalf("GetTableName mismatch: %q", got2)
	}
}

func TestRouterV2_AllShards_Cardinality(t *testing.T) {
	r := NewRouterV2WithConfig(2, 3)
	if got := len(r.GetAllShards()); got != 6 {
		t.Fatalf("expected 6 shards, got %d", got)
	}
	if got := len(r.GetDBShards(1)); got != 3 {
		t.Fatalf("expected 3 db-1 shards, got %d", got)
	}
}

// V1 / V2 layout 在表名后缀位数上必须不同 — cutover 期间避免两套表互覆。
func TestRouterV2_TableName_DiffersFromV1(t *testing.T) {
	v1 := NewRouter()
	v2 := NewRouterV2()
	t1 := v1.TableName(context.Background(), "voucher", 42)
	t2 := v2.TableName(context.Background(), "voucher", 0, 42)
	if t1 == t2 {
		t.Fatalf("V1 and V2 table names must differ: %q vs %q", t1, t2)
	}
	// V2 后缀必含 5 位 (xx_xxx)
	if !strings.Contains(t2, "_00_042") {
		t.Fatalf("V2 should have 5-digit suffix: %q", t2)
	}
}

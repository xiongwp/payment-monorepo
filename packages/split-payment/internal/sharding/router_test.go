package sharding

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRouter_RouteByString_Deterministic(t *testing.T) {
	r := NewRouter()
	key := "idem-abc-123"
	d1, t1 := r.RouteByString(key)
	d2, t2 := r.RouteByString(key)
	if d1 != d2 || t1 != t2 {
		t.Fatalf("不确定性: 同 key 两次 route 不一致 (%d,%d) vs (%d,%d)", d1, t1, d2, t2)
	}
}

func TestRouter_RouteByString_BoundedRange(t *testing.T) {
	r := NewRouter()
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("key-%d", i)
		d, tbl := r.RouteByString(key)
		if d < 0 || d >= r.DBCount() {
			t.Errorf("dbIdx 越界: key=%s → dbIdx=%d (期望 0..%d)", key, d, r.DBCount()-1)
		}
		if tbl < 0 || tbl >= r.TotalTables() {
			t.Errorf("tblIdx 越界: key=%s → tblIdx=%d (期望 0..%d)", key, tbl, r.TotalTables()-1)
		}
	}
}

// 验证：10000 个随机 idempotency-style key 散到 10 个 db，应该相对均匀
// （每个 db 大约 1000 个，允许 ±30%）。
func TestRouter_RouteByString_UniformDistribution(t *testing.T) {
	r := NewRouter()
	dbHits := make(map[int]int)
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("idem-%d-%s", i, strings.Repeat("x", i%5))
		d, _ := r.RouteByString(key)
		dbHits[d]++
	}
	expected := 10000 / r.DBCount()
	low, high := int(float64(expected)*0.7), int(float64(expected)*1.3)
	for d := 0; d < r.DBCount(); d++ {
		hits := dbHits[d]
		if hits < low || hits > high {
			t.Errorf("dbIdx=%d 命中 %d 次，超出预期范围 [%d, %d]", d, hits, low, high)
		}
	}
	t.Logf("10000 key 分布: %v", dbHits)
}

func TestRouter_RouteByInt(t *testing.T) {
	r := NewRouter()
	// id=0..99 应该刚好遍历 100 张全局表
	seen := make(map[int]bool)
	for id := int64(0); id < 100; id++ {
		_, tblIdx := r.RouteByInt(id)
		seen[tblIdx] = true
	}
	if len(seen) != 100 {
		t.Errorf("id 0..99 应该刚好遍历 100 张表，实际命中 %d", len(seen))
	}

	// 负数也能 route
	d, _ := r.RouteByInt(-12345)
	if d < 0 || d >= r.DBCount() {
		t.Errorf("负数 id route 越界: dbIdx=%d", d)
	}
}

func TestRouter_TableName_DBName(t *testing.T) {
	r := NewRouter()
	tests := []struct {
		family   string
		tblIdx   int
		shadow   bool
		wantName string
		dbIdx    int
		wantDB   string
	}{
		{"transfers", 7, false, "transfers_07", 0, "split_payment_db_0"},
		{"transfers", 7, true, "transfers_07_shadow", 0, "split_payment_db_0"},
		{"transfers", 99, false, "transfers_99", 9, "split_payment_db_9"},
		{"moneyflow_runs", 0, false, "moneyflow_runs_00", 0, "split_payment_db_0"},
		{"moneyflow_runs", 0, true, "moneyflow_runs_00_shadow", 0, "split_payment_db_0"},
		{"event_outbox", 55, false, "event_outbox_55", 5, "split_payment_db_5"},
	}
	for _, tt := range tests {
		got := r.TableName(tt.family, tt.tblIdx, tt.shadow)
		if got != tt.wantName {
			t.Errorf("TableName(%s, %d, shadow=%v) = %s, want %s",
				tt.family, tt.tblIdx, tt.shadow, got, tt.wantName)
		}
		gotDB := r.DBName(tt.dbIdx)
		if gotDB != tt.wantDB {
			t.Errorf("DBName(%d) = %s, want %s", tt.dbIdx, gotDB, tt.wantDB)
		}
	}
}

func TestRouter_TableNameCtx_Shadow(t *testing.T) {
	r := NewRouter()
	// 默认 ctx → 主表
	plain := r.TableNameCtx(context.Background(), "transfers", 7)
	if plain != "transfers_07" {
		t.Errorf("plain ctx → %s, want transfers_07", plain)
	}
	// shadow ctx → _shadow 表
	shadowCtx := WithShadow(context.Background())
	shadow := r.TableNameCtx(shadowCtx, "transfers", 7)
	if shadow != "transfers_07_shadow" {
		t.Errorf("shadow ctx → %s, want transfers_07_shadow", shadow)
	}
}

func TestRouter_Resolve(t *testing.T) {
	r := NewRouter()
	dbName, tableName, dbIdx := r.Resolve(context.Background(), "transfers", "idem-abc-123")
	// 验证 db 名格式 + table 名格式 + dbIdx 在范围内
	if !strings.HasPrefix(dbName, "split_payment_db_") {
		t.Errorf("dbName 格式异常: %s", dbName)
	}
	if !strings.HasPrefix(tableName, "transfers_") {
		t.Errorf("tableName 格式异常: %s", tableName)
	}
	if dbIdx < 0 || dbIdx >= r.DBCount() {
		t.Errorf("dbIdx 越界: %d", dbIdx)
	}
	if strings.HasSuffix(tableName, "_shadow") {
		t.Errorf("plain ctx 不应拼 _shadow: %s", tableName)
	}
	t.Logf("Resolve(transfers, idem-abc-123) → %s.%s (dbIdx=%d)", dbName, tableName, dbIdx)

	// shadow ctx → _shadow 后缀
	shadowCtx := WithShadow(context.Background())
	_, shadowTbl, _ := r.Resolve(shadowCtx, "transfers", "idem-abc-123")
	if !strings.HasSuffix(shadowTbl, "_shadow") {
		t.Errorf("shadow ctx 表名应以 _shadow 结尾: %s", shadowTbl)
	}
}

func TestRouter_AllTables_Coverage(t *testing.T) {
	r := NewRouter()
	tables := r.AllTables(context.Background(), "payouts")
	if len(tables) != r.TotalTables() {
		t.Errorf("AllTables 应返 %d 张表，实际 %d", r.TotalTables(), len(tables))
	}
	// 每个 db 应该有 tablePerDB 张表
	dbCounts := make(map[int]int)
	for _, st := range tables {
		dbCounts[st.DBIdx]++
	}
	for d, n := range dbCounts {
		if n != r.TablePerDB() {
			t.Errorf("db_%d 含 %d 张表，期望 %d", d, n, r.TablePerDB())
		}
	}
}

func TestRouter_AllTables_ShadowCtx(t *testing.T) {
	r := NewRouter()
	shadowCtx := WithShadow(context.Background())
	tables := r.AllTables(shadowCtx, "payouts")
	for _, st := range tables {
		if !strings.HasSuffix(st.TableName, "_shadow") {
			t.Errorf("shadow ctx 下 AllTables 应该都拼 _shadow, got %s", st.TableName)
		}
	}
}

func TestShadowCtx(t *testing.T) {
	if IsShadow(nil) {
		t.Error("nil ctx 应该 IsShadow=false")
	}
	if IsShadow(context.Background()) {
		t.Error("plain ctx 应该 IsShadow=false")
	}
	if !IsShadow(WithShadow(context.Background())) {
		t.Error("WithShadow 后应该 IsShadow=true")
	}
}

// 空字符串退化到 (0, 0) 不 panic
func TestRouter_RouteByString_EmptyKey(t *testing.T) {
	r := NewRouter()
	d, tbl := r.RouteByString("")
	if d != 0 || tbl != 0 {
		t.Errorf("空 key 应退化到 (0,0)，实际 (%d,%d)", d, tbl)
	}
}

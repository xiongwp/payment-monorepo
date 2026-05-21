package repository

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
)

// ============================================================================
// 路由测试：anchor 表的分片是路由层的核心，错一片所有 anchor 查询会路由到错的
// 表 → 业务上等于丢失锚点 → I-A1 不变量灾难。必须穷举验证。
// ============================================================================

func TestHashStringMod100_RangeAndDeterminism(t *testing.T) {
	cases := []string{
		"PAY_001", "PAY_002", "refund_xyz",
		"00000000-0000-0000-0000-000000000000",
		"order:alipay:20260521:100001",
		"超长请求ID:" + strings.Repeat("x", 50),
		"",
	}
	for _, c := range cases {
		got := hashStringMod100(c)
		if got < 0 || got >= sharding.ShardTableTotal {
			t.Errorf("hash(%q) = %d out of range [0, %d)", c, got, sharding.ShardTableTotal)
		}
		// 同一输入必须每次同结果（FNV 是确定性的）
		again := hashStringMod100(c)
		if again != got {
			t.Errorf("hash non-deterministic for %q: %d vs %d", c, got, again)
		}
	}
}

func TestRouteByRequestID_ConsistentWithFNV(t *testing.T) {
	r := &anchorRepository{}
	for _, reqID := range []string{"PAY_001", "RFD_99", "tx-abc"} {
		dbIdx, gtblIdx := r.RouteByRequestID(reqID)
		// invariant: dbIdx = gtblIdx / 10
		if dbIdx != gtblIdx/sharding.ShardTablePerDB {
			t.Errorf("RouteByRequestID(%q) = (db=%d, gtbl=%d) violates db = gtbl/%d",
				reqID, dbIdx, gtblIdx, sharding.ShardTablePerDB)
		}
		if dbIdx < 0 || dbIdx >= sharding.ShardDBCount {
			t.Errorf("RouteByRequestID(%q) dbIdx %d out of range", reqID, dbIdx)
		}
	}
}

func TestRouteByRequestID_EmptyFailsafe(t *testing.T) {
	r := &anchorRepository{}
	db, tbl := r.RouteByRequestID("")
	if db != 0 || tbl != 0 {
		t.Errorf("empty request_id should fail-safe to (0,0), got (%d,%d)", db, tbl)
	}
}

// 分布均匀性：FNV-1a 对随机 string 应该接近均匀。我们生成 10000 个 reqID，每个
// 桶平均应有 ~100 笔，允许 ±50% 浮动。这是一个软性检测，主要是抓"全部落到 shard 0"
// 这类灾难性 bug。
func TestRouteByRequestID_DistributionRoughlyUniform(t *testing.T) {
	r := &anchorRepository{}
	const N = 10000
	buckets := make([]int, sharding.ShardTableTotal)
	for i := 0; i < N; i++ {
		_, gtbl := r.RouteByRequestID(fmt.Sprintf("REQ_%08d_%d", i, i*7919))
		buckets[gtbl]++
	}
	expected := N / sharding.ShardTableTotal
	lo := expected / 2
	hi := expected * 2
	for tbl, cnt := range buckets {
		if cnt < lo || cnt > hi {
			t.Errorf("shard %d got %d hits, expected ~%d (range [%d,%d])",
				tbl, cnt, expected, lo, hi)
		}
	}
}

// ============================================================================
// isDuplicateKeyErr 测试：MySQL duplicate key 是 anchor 并发首次锚定的关键边界
// （E-01）。错过这个分支会让上层拿到 ErrAnchorAlreadyExists 而不是泛错误，
// 业务退化路径会失效。
// ============================================================================

func TestIsDuplicateKeyErr_DetectsCommonForms(t *testing.T) {
	cases := []struct {
		err      error
		expected bool
	}{
		{nil, false},
		{errors.New("some random error"), false},
		{errors.New("Error 1062: Duplicate entry 'foo' for key 'uk_req_logical'"), true},
		{errors.New("MySQL: Duplicate entry"), true},
		{errors.New("connection refused"), false},
		{errors.New("constraint violation: duplicate key value"), true},
		// 真正的 GORM wrap 可能在前面加 "Error" 字眼，但 1062 是稳定的
		{errors.New("driver: bad connection / Error 1062"), true},
	}
	for _, c := range cases {
		got := isDuplicateKeyErr(c.err)
		if got != c.expected {
			t.Errorf("isDuplicateKeyErr(%v) = %v, want %v", c.err, got, c.expected)
		}
	}
}

func TestContains_BasicCases(t *testing.T) {
	cases := []struct {
		s, substr string
		want      bool
	}{
		{"hello world", "world", true},
		{"hello world", "Hello", false},
		{"abc", "", true},
		{"", "", true},
		{"", "x", false},
		{"abcdef", "abcdef", true},
		{"abc", "abcd", false},
	}
	for _, c := range cases {
		if got := contains(c.s, c.substr); got != c.want {
			t.Errorf("contains(%q, %q) = %v, want %v", c.s, c.substr, got, c.want)
		}
	}
}

// ============================================================================
// Shard 边界
// ============================================================================

func TestShardTableName_RejectsOutOfRange(t *testing.T) {
	r := &anchorRepository{router: sharding.NewRouter()}
	for _, bad := range []int{-1, sharding.ShardTableTotal, sharding.ShardTableTotal + 1, 9999} {
		_, err := r.shardTableName(nil, bad) //nolint:staticcheck // nil ctx OK for boundary check
		if err == nil {
			t.Errorf("expected error for gtbl=%d, got nil", bad)
		}
		if !errors.Is(err, ErrAnchorShardMisrouted) {
			t.Errorf("expected ErrAnchorShardMisrouted, got %v", err)
		}
	}
}

func TestShardTableName_AcceptsValid(t *testing.T) {
	r := &anchorRepository{router: sharding.NewRouter()}
	for _, ok := range []int{0, 1, 50, 99} {
		name, err := r.shardTableName(nil, ok) //nolint:staticcheck // nil ctx OK for boundary check
		if err != nil {
			t.Errorf("expected no error for gtbl=%d, got %v", ok, err)
		}
		if !strings.HasPrefix(name, "tx_account_anchor_") {
			t.Errorf("table name missing prefix: %q", name)
		}
	}
}

// ============================================================================
// EDGE CASES — 补充覆盖
// ============================================================================

// FNV-1a 在单调递增输入上不应出现退化（避免相邻请求落同一片）。
// 这种模式实际发生在：用 timestamp 后缀的 request_id，多笔交易聚集在毫秒级。
func TestHash_SequentialInputsNoSamePartitionRun(t *testing.T) {
	// 模拟 1000 个相邻 timestamp 的 reqID，检查任意 50 个连续中不会全落同片
	const N = 1000
	const windowSize = 50
	const maxSameShard = 8 // 同片不超过 8/50 = 16%（随机分布期望 0.5 ≈ 1）
	buckets := make([]int, N)
	for i := 0; i < N; i++ {
		buckets[i] = hashStringMod100(fmt.Sprintf("PAY_1748880000%06d", i))
	}
	for start := 0; start+windowSize < N; start++ {
		freq := make(map[int]int)
		for j := 0; j < windowSize; j++ {
			freq[buckets[start+j]]++
		}
		for shard, cnt := range freq {
			if cnt > maxSameShard {
				t.Errorf("hash sequential input degenerate: window=[%d..%d] shard=%d count=%d (max allowed=%d)",
					start, start+windowSize, shard, cnt, maxSameShard)
				return // 一次失败就够诊断
			}
		}
	}
}

// FNV-1a 对 UUID 样式输入分布均匀
func TestHash_UUIDStyleDistribution(t *testing.T) {
	// 生成 5000 个 UUID 风格的 reqID
	const N = 5000
	buckets := make([]int, sharding.ShardTableTotal)
	for i := 0; i < N; i++ {
		req := fmt.Sprintf("%08x-%04x-4%03x-%04x-%012x",
			i, i*7, i*13, i*17, i*23)
		_, gtbl := (&anchorRepository{}).RouteByRequestID(req)
		buckets[gtbl]++
	}
	exp := N / sharding.ShardTableTotal
	for tbl, cnt := range buckets {
		if cnt < exp/2 || cnt > exp*2 {
			t.Errorf("UUID-style shard %d cnt=%d expected ~%d", tbl, cnt, exp)
		}
	}
}

// isDuplicateKeyErr 必须能识别 fmt.Errorf("...: %w", innerErr) 这种 wrapped error。
// 因为 GORM 在内部经常 wrap driver 错误，我们的判别必须穿透。
func TestIsDuplicateKeyErr_WrappedErrors(t *testing.T) {
	inner := errors.New("Error 1062: Duplicate entry 'PAY_001-42' for key 'uk_req_logical'")
	wrapped := fmt.Errorf("repository.Anchor.Insert: %w", inner)
	if !isDuplicateKeyErr(wrapped) {
		t.Error("must detect 1062 even when wrapped by fmt.Errorf %w")
	}

	// 双层包装
	doubleWrapped := fmt.Errorf("service.Book: %w", wrapped)
	if !isDuplicateKeyErr(doubleWrapped) {
		t.Error("must detect 1062 through multiple wrap layers")
	}

	// 错误链里既有 1062 字样又有别的：仍判 true
	mixed := errors.New("connection ok, then Error 1062: Duplicate entry; retrying")
	if !isDuplicateKeyErr(mixed) {
		t.Error("must detect 1062 substring even mixed with other text")
	}
}

// shardTableName 极端边界 + 0/99
func TestShardTableName_ExtremeBoundaries(t *testing.T) {
	r := &anchorRepository{router: sharding.NewRouter()}
	// MinInt / MaxInt 必须拒，不能 panic
	for _, bad := range []int{-2147483648, 2147483647} {
		_, err := r.shardTableName(nil, bad) //nolint:staticcheck
		if err == nil {
			t.Errorf("extreme value %d should error", bad)
		}
		if !errors.Is(err, ErrAnchorShardMisrouted) {
			t.Errorf("extreme value %d should ErrAnchorShardMisrouted, got %v", bad, err)
		}
	}
}

// dbForShard 同样的边界
func TestDBForShard_ExtremeBoundaries(t *testing.T) {
	r := &anchorRepository{router: sharding.NewRouter()}
	for _, bad := range []int{-1, sharding.ShardTableTotal, 9999} {
		_, err := r.dbForShard(bad)
		if err == nil {
			t.Errorf("dbForShard(%d) should error", bad)
		}
		if !errors.Is(err, ErrAnchorShardMisrouted) {
			t.Errorf("dbForShard(%d): expected ErrAnchorShardMisrouted, got %v", bad, err)
		}
	}
}

// RouteByRequestID 必须返回的 (dbIdx, gtblIdx) 满足 dbIdx = gtblIdx / 10
// 在 10000 个随机请求里穷举验证（防止未来 sharding.Router 调整时本约定漂移）。
func TestRouteByRequestID_DbIdxConsistentWithGTbl(t *testing.T) {
	r := &anchorRepository{}
	for i := 0; i < 10000; i++ {
		req := fmt.Sprintf("REQ%d", i)
		dbIdx, gtblIdx := r.RouteByRequestID(req)
		if dbIdx != gtblIdx/sharding.ShardTablePerDB {
			t.Errorf("req=%q dbIdx=%d gtbl=%d: db should be gtbl/%d=%d",
				req, dbIdx, gtblIdx, sharding.ShardTablePerDB, gtblIdx/sharding.ShardTablePerDB)
		}
	}
}

// contains 多字节字符 / unicode 边界（虽然 anchor 错误检测主要是 ASCII，但稳健性测试）
func TestContains_Unicode(t *testing.T) {
	if !contains("Error 1062: 重复键值 'foo'", "1062") {
		t.Error("must detect 1062 in unicode-containing string")
	}
	if !contains("Duplicate entry", "Dup") {
		t.Error("prefix match")
	}
}

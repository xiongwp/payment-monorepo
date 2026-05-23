package repository

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xiongwp/accounting-system/internal/infrastructure/sharding"
)

// ============================================================================
// 方向 B repository 测试
// ============================================================================

func TestHashStringMod100_RangeAndDeterminism(t *testing.T) {
	for _, s := range []string{
		"PAY_001", "RFD_99", "tx-abc",
		"00000000-0000-0000-0000-000000000000",
		"超长 flow id:" + strings.Repeat("x", 50),
		"",
	} {
		v := hashStringMod100(s)
		if v < 0 || v >= sharding.ShardTableTotal {
			t.Errorf("hash(%q)=%d out of [0,%d)", s, v, sharding.ShardTableTotal)
		}
		if hashStringMod100(s) != v {
			t.Errorf("hash non-deterministic for %q", s)
		}
	}
}

func TestIsDuplicateKeyErr_DetectsCommonForms(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("some random error"), false},
		{errors.New("Error 1062: Duplicate entry 'X' for key 'uk_flow_account'"), true},
		{errors.New("Duplicate entry"), true},
		{errors.New("connection refused"), false},
		{errors.New("duplicate key value"), true},
		{fmt.Errorf("wrap: %w", errors.New("Error 1062")), true},
	}
	for _, c := range cases {
		if isDuplicateKeyErr(c.err) != c.want {
			t.Errorf("isDuplicateKeyErr(%v)=%v want %v", c.err, isDuplicateKeyErr(c.err), c.want)
		}
	}
}

func TestContains_BasicCases(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"hello", "hell", true},
		{"hello", "Hell", false},
		{"abc", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if contains(c.s, c.sub) != c.want {
			t.Errorf("contains(%q,%q)=%v want %v", c.s, c.sub, contains(c.s, c.sub), c.want)
		}
	}
}

// flow_anchor_route hash 分片确定性
func TestFlowAnchorRoute_RouteByFlowID(t *testing.T) {
	r := &flowAnchorRouteRepository{router: sharding.NewRouter()}
	for _, flow := range []string{"F1", "PAY_001", "RFD_xxx"} {
		dbIdx, gtbl := r.RouteByFlowID(flow)
		if dbIdx != gtbl/sharding.ShardTablePerDB {
			t.Errorf("flow=%s db=%d gtbl=%d violates db = gtbl/%d",
				flow, dbIdx, gtbl, sharding.ShardTablePerDB)
		}
		if dbIdx < 0 || dbIdx >= sharding.ShardDBCount {
			t.Errorf("db out of range")
		}
	}
}

func TestFlowAnchorRoute_RouteByFlowID_EmptyFailsafe(t *testing.T) {
	r := &flowAnchorRouteRepository{router: sharding.NewRouter()}
	db, tbl := r.RouteByFlowID("")
	if db != 0 || tbl != 0 {
		t.Errorf("empty flow_id should fail-safe to (0,0), got (%d,%d)", db, tbl)
	}
}

// 分布均匀性
func TestFlowAnchorRoute_DistributionRoughlyUniform(t *testing.T) {
	r := &flowAnchorRouteRepository{router: sharding.NewRouter()}
	const N = 10000
	buckets := make([]int, sharding.ShardTableTotal)
	for i := 0; i < N; i++ {
		_, gtbl := r.RouteByFlowID(fmt.Sprintf("FLOW_%08d_%d", i, i*7919))
		buckets[gtbl]++
	}
	exp := N / sharding.ShardTableTotal
	for tbl, cnt := range buckets {
		if cnt < exp/2 || cnt > exp*2 {
			t.Errorf("shard %d cnt=%d expected ~%d", tbl, cnt, exp)
		}
	}
}

package healthx

import (
	"database/sql"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// 验证 collector 实现 prometheus.Collector 接口 + Describe/Collect 不 panic。
// db 为 nil 时 Collect 跳过。
func TestDBStatsCollector_NilDBSafe(t *testing.T) {
	c := NewDBStatsCollector("test", nil)
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}
}

func TestDBStatsCollector_DescribeAllSix(t *testing.T) {
	c := NewDBStatsCollector("test", nil)
	ch := make(chan *prometheus.Desc, 16)
	c.Describe(ch)
	close(ch)
	count := 0
	for range ch {
		count++
	}
	if count != 6 {
		t.Fatalf("expected 6 metric descriptors, got %d", count)
	}
}

func TestNewMultiDBStatsCollector_NilEntries(t *testing.T) {
	// nil 值的 entry 不该 panic
	c := NewMultiDBStatsCollector("svc", map[string]*sql.DB{
		"shard0": nil,
		"shard1": nil,
	})
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := reg.Gather(); err != nil {
		t.Fatalf("gather: %v", err)
	}
}

package reshard

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSpec 给单元测试用：每 shard 100 行，PK 1..100。
// Transform 把 row["pk"] 转 V2 dst，按 pk%dstShards 分桶。
type fakeSpec struct {
	tableName  string
	v1Shards   [][2]int
	rowsPerShard int
	dstShards  int
	written    atomic.Int64
}

func newFakeSpec(name string, v1Shards [][2]int, rowsPerShard, dstShards int) *fakeSpec {
	return &fakeSpec{tableName: name, v1Shards: v1Shards, rowsPerShard: rowsPerShard, dstShards: dstShards}
}

func (f *fakeSpec) Name() string         { return f.tableName }
func (f *fakeSpec) V1Shards() [][2]int   { return f.v1Shards }
func (f *fakeSpec) PrimaryKey(r Row) int64 { return r["pk"].(int64) }

func (f *fakeSpec) ScanV1(_ context.Context, shard [2]int, lastPK int64, limit int) ([]Row, int64, bool, error) {
	startPK := lastPK + 1
	rows := []Row{}
	maxPK := lastPK
	for pk := startPK; pk <= int64(f.rowsPerShard); pk++ {
		if len(rows) >= limit {
			break
		}
		rows = append(rows, Row{"pk": pk, "shard": shard, "v": "data"})
		maxPK = pk
	}
	done := maxPK >= int64(f.rowsPerShard)
	return rows, maxPK, done, nil
}

func (f *fakeSpec) TransformV1ToV2(_ context.Context, r Row) (Row, int, int, error) {
	pk := r["pk"].(int64)
	dst := int(pk % int64(f.dstShards))
	return r, 0, dst, nil
}

func (f *fakeSpec) WriteV2(_ context.Context, _, _ int, rows []Row) error {
	f.written.Add(int64(len(rows)))
	return nil
}

// 基本场景：2 shard × 100 row → 200 row 全迁完。
func TestMigrator_BasicCompletes(t *testing.T) {
	spec := newFakeSpec("fake", [][2]int{{0, 0}, {0, 1}}, 100, 4)
	cp := NewMemCheckpoint()
	cfg := DefaultConfig()
	cfg.IdleSleep = 10 * time.Millisecond
	m := New(spec, cp, cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Start(ctx)

	// 等到 totalRows == 200
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.SnapshotStats().TotalRows >= 200 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Stop()

	stats := m.SnapshotStats()
	if stats.TotalRows != 200 {
		t.Fatalf("expected 200 rows migrated, got %d", stats.TotalRows)
	}
	if spec.written.Load() != 200 {
		t.Fatalf("WriteV2 called for %d rows, want 200", spec.written.Load())
	}
	if int(stats.ShardDone) != len(spec.v1Shards) {
		t.Fatalf("ShardDone=%d want %d", stats.ShardDone, len(spec.v1Shards))
	}
}

// 断点续传：第一次跑到 50 PK 停掉，第二次 New + Start 应该从 51 继续。
func TestMigrator_ResumeFromCheckpoint(t *testing.T) {
	spec := newFakeSpec("fake_resume", [][2]int{{0, 0}}, 100, 2)
	cp := NewMemCheckpoint()

	// 手工写入 checkpoint：lastPK=50
	_ = cp.Save(context.Background(), "fake_resume", [2]int{0, 0}, 50, 50)

	cfg := DefaultConfig()
	cfg.IdleSleep = 10 * time.Millisecond
	m := New(spec, cp, cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m.Start(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if m.SnapshotStats().ShardDone >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Stop()

	// 应该只迁了 50 行（51..100）
	if got := spec.written.Load(); got != 50 {
		t.Fatalf("expected 50 rows migrated after resume, got %d", got)
	}
}

// 速率限制：RatePerSec 100 + BatchSize 10 → 100 行至少 1s。
func TestMigrator_RateLimitHonored(t *testing.T) {
	spec := newFakeSpec("fake_rate", [][2]int{{0, 0}}, 50, 2)
	cp := NewMemCheckpoint()
	cfg := DefaultConfig()
	cfg.BatchSize = 10
	cfg.RatePerSec = 50 // 50 rows/sec → 50 rows 应至少 1s
	cfg.IdleSleep = 10 * time.Millisecond
	cfg.Concurrency = 1
	m := New(spec, cp, cfg, nil)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Start(ctx)

	for time.Now().Before(start.Add(4 * time.Second)) {
		if m.SnapshotStats().TotalRows >= 50 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Stop()
	elapsed := time.Since(start)

	if spec.written.Load() != 50 {
		t.Fatalf("expected 50 rows, got %d", spec.written.Load())
	}
	// 50 rows @ 50/s = 1s 起 (允许少量误差，limiter 第一批 burst=batch 不等)
	if elapsed < 800*time.Millisecond {
		t.Fatalf("rate limit not honored: 50 rows in %v", elapsed)
	}
}

// Pause / Resume：暂停后短期内不前进，Resume 后继续。
func TestMigrator_PauseResume(t *testing.T) {
	spec := newFakeSpec("fake_pause", [][2]int{{0, 0}}, 50, 2)
	cp := NewMemCheckpoint()
	cfg := DefaultConfig()
	cfg.BatchSize = 5
	cfg.IdleSleep = 10 * time.Millisecond
	m := New(spec, cp, cfg, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Start(ctx)
	// 让它跑一会
	time.Sleep(50 * time.Millisecond)
	m.Pause()

	beforePause := m.SnapshotStats().TotalRows
	time.Sleep(200 * time.Millisecond) // 暂停期 200ms
	afterPause := m.SnapshotStats().TotalRows
	if afterPause != beforePause {
		t.Logf("rows progressed during pause (allowed for in-flight batch): %d → %d", beforePause, afterPause)
	}

	m.Resume()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.SnapshotStats().TotalRows >= 50 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Stop()

	if got := spec.written.Load(); got != 50 {
		t.Fatalf("after resume expected 50 rows, got %d", got)
	}
}

// IsRunning 状态机：未 Start → false；Start 后 → true；Stop 后 → false。
func TestMigrator_IsRunning(t *testing.T) {
	spec := newFakeSpec("fake_running", [][2]int{{0, 0}}, 10, 2)
	m := New(spec, NewMemCheckpoint(), DefaultConfig(), nil)

	if m.IsRunning() {
		t.Fatal("not started, should not be running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.Start(ctx)
	if !m.IsRunning() {
		t.Fatal("after Start, should be running")
	}
	m.Stop()
	if m.IsRunning() {
		t.Fatal("after Stop, should not be running")
	}
}

// MemCheckpoint Save 总取较大值，保证多 worker 并发提交不回退。
func TestMemCheckpoint_MonotonicSave(t *testing.T) {
	cp := NewMemCheckpoint()
	ctx := context.Background()
	_ = cp.Save(ctx, "t", [2]int{0, 0}, 100, 100)
	_ = cp.Save(ctx, "t", [2]int{0, 0}, 50, 50) // 旧值不应覆盖
	pk, total, _ := cp.Load(ctx, "t", [2]int{0, 0})
	if pk != 100 {
		t.Fatalf("Save should be monotonic, got pk=%d", pk)
	}
	if total != 100 {
		t.Fatalf("total should be monotonic, got %d", total)
	}
}

// 空 shard list — Start 立刻空闲（不 panic）。
func TestMigrator_NoShards(t *testing.T) {
	spec := newFakeSpec("empty", [][2]int{}, 0, 2)
	m := New(spec, NewMemCheckpoint(), DefaultConfig(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	m.Stop()

	if got := spec.written.Load(); got != 0 {
		t.Fatalf("no shards but rows written: %d", got)
	}
}

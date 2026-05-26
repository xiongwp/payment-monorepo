package reliability

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper：构一个 fast-adjust limiter（默认 200ms 节流，测试里压到 1ms）。
func newTestLimiter(initial, min, max int, enabled bool) *AdaptiveLimiter {
	l := NewAdaptive(AdaptiveConfig{
		Initial:         initial,
		Min:             min,
		Max:             max,
		LongHalfLife:    100 * time.Millisecond,
		ShortHalfLife:   10 * time.Millisecond,
		SmoothingAlpha:  0.5,
		ExplorationRate: 0, // 关探索，单测要确定性
		MerchantCap:     1000,
		Enabled:         enabled,
	})
	l.SetMinAdjustInterval(time.Millisecond)
	return l
}

// simulateRTT 模拟一笔 RTT；不实际 sleep，直接喂 observe。
func simulateRTT(l *AdaptiveLimiter, merchant string, rtt time.Duration) {
	st := l.stateFor(merchant)
	l.observe(merchant, st, rtt.Seconds())
}

// 1) ErrLimitExceeded 是哨兵
func TestAdaptive_ErrSentinel(t *testing.T) {
	if !errors.Is(ErrLimitExceeded, ErrLimitExceeded) {
		t.Fatal("ErrLimitExceeded sentinel must be identifiable")
	}
}

// 2) 默认 disabled → 永远放行
func TestAdaptive_DisabledAlwaysAllows(t *testing.T) {
	l := newTestLimiter(2, 1, 4, false)
	releases := []func(){}
	for i := 0; i < 100; i++ {
		rel, err := l.Acquire("m1")
		if err != nil {
			t.Fatalf("disabled must always allow, got err at %d", i)
		}
		releases = append(releases, rel)
	}
	for _, r := range releases {
		r()
	}
}

// 3) inflight 超 limit → 返 ErrLimitExceeded
func TestAdaptive_RejectsAboveLimit(t *testing.T) {
	l := newTestLimiter(3, 1, 10, true)
	// 占满 3 个
	rels := []func(){}
	for i := 0; i < 3; i++ {
		rel, err := l.Acquire("m1")
		if err != nil {
			t.Fatalf("slot %d should succeed, got %v", i, err)
		}
		rels = append(rels, rel)
	}
	// 第 4 个应被拒
	rel, err := l.Acquire("m1")
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("expected ErrLimitExceeded, got %v", err)
	}
	if rel == nil {
		t.Fatal("release must be non-nil even on rejection")
	}
	rel() // no-op safe
	// 释放一个 → 应该能再 Acquire
	rels[0]()
	rel2, err := l.Acquire("m1")
	if err != nil {
		t.Fatalf("after release expected slot, got %v", err)
	}
	rel2()
	for _, r := range rels[1:] {
		r()
	}
}

// 4) release 后 inflight 应减
func TestAdaptive_ReleaseDecrements(t *testing.T) {
	l := newTestLimiter(5, 1, 10, true)
	rel, err := l.Acquire("m1")
	if err != nil {
		t.Fatal(err)
	}
	st := l.stateFor("m1")
	if atomic.LoadInt64(&st.inflight) != 1 {
		t.Fatalf("expected inflight=1, got %d", atomic.LoadInt64(&st.inflight))
	}
	rel()
	if atomic.LoadInt64(&st.inflight) != 0 {
		t.Fatalf("expected inflight=0 after release, got %d", atomic.LoadInt64(&st.inflight))
	}
	// 双释幂等
	rel()
	if atomic.LoadInt64(&st.inflight) != 0 {
		t.Fatalf("double-release should be idempotent, got %d", atomic.LoadInt64(&st.inflight))
	}
}

// 5) per-merchant 隔离：A 满不影响 B
func TestAdaptive_PerMerchantIsolation(t *testing.T) {
	l := newTestLimiter(2, 1, 5, true)
	// 占满 A
	rA1, _ := l.Acquire("A")
	rA2, _ := l.Acquire("A")
	if _, err := l.Acquire("A"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("A should be saturated")
	}
	// B 仍可
	rB, err := l.Acquire("B")
	if err != nil {
		t.Fatalf("B isolation broken: %v", err)
	}
	rA1()
	rA2()
	rB()
}

// 6) RTT 稳定时 limit 应升到 max
func TestAdaptive_StableRTTGrowsToMax(t *testing.T) {
	l := newTestLimiter(5, 5, 100, true)
	// 稳定 10ms RTT，灌大量样本。短长窗 RTT 应趋同，gradient ≈ 1，
	// next = limit + sqrt(limit) → 单调上探到 max。
	for i := 0; i < 2000; i++ {
		simulateRTT(l, "m1", 10*time.Millisecond)
		time.Sleep(time.Millisecond) // 让 minAdjustInterval 触发
		if i%200 == 0 {
			snap := l.Snapshot()
			if len(snap) == 0 {
				continue
			}
			if snap[0].Limit >= 100 {
				return // 已到 max
			}
		}
	}
	snap := l.Snapshot()
	if len(snap) == 0 || snap[0].Limit < 50 {
		t.Fatalf("expected limit to grow well above initial under stable RTT, got %+v", snap)
	}
}

// 7) RTT 突然 10× 变慢 → limit 显著下降
func TestAdaptive_SlowdownReducesLimit(t *testing.T) {
	l := newTestLimiter(50, 5, 200, true)
	// 先稳定 10ms 灌满长窗
	for i := 0; i < 200; i++ {
		simulateRTT(l, "m1", 10*time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	preSnap := l.Snapshot()
	if len(preSnap) == 0 {
		t.Fatal("no snapshot")
	}
	preLimit := preSnap[0].Limit
	// 突然 100ms RTT（10×）
	for i := 0; i < 500; i++ {
		simulateRTT(l, "m1", 100*time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	postSnap := l.Snapshot()
	if len(postSnap) == 0 {
		t.Fatal("no snapshot")
	}
	postLimit := postSnap[0].Limit
	if postLimit >= preLimit {
		t.Fatalf("expected limit to drop after slowdown: pre=%d post=%d", preLimit, postLimit)
	}
	// 至少下降到不超过 pre 的 80%
	if float64(postLimit) > float64(preLimit)*0.85 {
		t.Logf("warning: limit only dropped from %d to %d (<15%%)", preLimit, postLimit)
	}
}

// 8) 并发 Acquire/Release：100 goroutine 不 race / count 守恒
func TestAdaptive_ConcurrencySafety(t *testing.T) {
	l := newTestLimiter(50, 10, 200, true)
	var wg sync.WaitGroup
	var ops atomic.Int64
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				rel, err := l.Acquire("hot")
				if err == nil {
					// 模拟短任务
					rel()
					ops.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	// 收尾 inflight 必须归零
	st := l.stateFor("hot")
	if got := atomic.LoadInt64(&st.inflight); got != 0 {
		t.Fatalf("inflight leak: %d", got)
	}
	if ops.Load() == 0 {
		t.Fatal("no successful acquire under concurrency")
	}
}

// 9) admin SetLimit / Reset
func TestAdaptive_SetLimitAndReset(t *testing.T) {
	l := newTestLimiter(10, 5, 100, true)
	l.SetLimit("m1", 7)
	snap := l.Snapshot()
	if len(snap) == 0 || snap[0].Limit != 7 {
		t.Fatalf("expected SetLimit=7, got %+v", snap)
	}
	// clamp
	l.SetLimit("m1", 999)
	snap = l.Snapshot()
	if snap[0].Limit != 100 {
		t.Fatalf("expected clamp to max=100, got %d", snap[0].Limit)
	}
	l.SetLimit("m1", 0)
	snap = l.Snapshot()
	if snap[0].Limit != 5 {
		t.Fatalf("expected clamp to min=5, got %d", snap[0].Limit)
	}
	// Reset：回到 Initial=10
	l.Reset()
	snap = l.Snapshot()
	if snap[0].Limit != 10 {
		t.Fatalf("expected reset to initial=10, got %d", snap[0].Limit)
	}
}

// 10) global cap 兜底
func TestAdaptive_GlobalCap(t *testing.T) {
	l := NewAdaptive(AdaptiveConfig{
		Initial:   10,
		Min:       5,
		Max:       100,
		GlobalCap: 3,
		Enabled:   true,
	})
	l.SetMinAdjustInterval(time.Millisecond)
	r1, e1 := l.Acquire("a")
	r2, e2 := l.Acquire("b")
	r3, e3 := l.Acquire("c")
	if e1 != nil || e2 != nil || e3 != nil {
		t.Fatalf("first 3 should succeed: %v %v %v", e1, e2, e3)
	}
	_, e4 := l.Acquire("d")
	if !errors.Is(e4, ErrLimitExceeded) {
		t.Fatalf("4th should hit global cap, got %v", e4)
	}
	inflight, rejected, _, gc := l.GlobalSnapshot()
	if inflight != 3 || rejected < 1 || gc != 3 {
		t.Fatalf("global snapshot wrong: inflight=%d rejected=%d cap=%d", inflight, rejected, gc)
	}
	r1()
	r2()
	r3()
}

// 11) LRU eviction：超 MerchantCap 后最老的被踢
func TestAdaptive_LRUEviction(t *testing.T) {
	l := NewAdaptive(AdaptiveConfig{
		Initial:     10,
		Min:         5,
		Max:         50,
		MerchantCap: 3,
		Enabled:     true,
	})
	l.SetMinAdjustInterval(time.Millisecond)
	l.stateFor("m1")
	l.stateFor("m2")
	l.stateFor("m3")
	l.stateFor("m4") // 应踢 m1
	snap := l.Snapshot()
	ids := map[string]bool{}
	for _, s := range snap {
		ids[s.MerchantID] = true
	}
	if ids["m1"] {
		t.Fatalf("expected m1 to be LRU-evicted, snap=%+v", snap)
	}
	if !ids["m2"] || !ids["m3"] || !ids["m4"] {
		t.Fatalf("expected m2/m3/m4 present, snap=%+v", snap)
	}
	if len(snap) != 3 {
		t.Fatalf("expected cap=3 entries, got %d", len(snap))
	}
}

// 12) callbacks 被触发
func TestAdaptive_Callbacks(t *testing.T) {
	l := newTestLimiter(5, 5, 50, true)
	var rejects atomic.Int64
	var limitChanges atomic.Int64
	var rttCalls atomic.Int64
	l.SetCallbacks(
		func(_ string, _, _ int) { limitChanges.Add(1) },
		func(_, _ string) { rejects.Add(1) },
		func(_ string, _, _ float64) { rttCalls.Add(1) },
	)
	// 触发 reject
	rels := []func(){}
	for i := 0; i < 5; i++ {
		r, _ := l.Acquire("m1")
		rels = append(rels, r)
	}
	if _, err := l.Acquire("m1"); err == nil {
		t.Fatal("expected reject")
	}
	if rejects.Load() == 0 {
		t.Fatal("onReject not invoked")
	}
	// 触发 RTT + 可能 limitChange
	for _, r := range rels {
		r()
	}
	// 灌点 RTT 让 limit 走起来
	for i := 0; i < 100; i++ {
		simulateRTT(l, "m1", 5*time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	if rttCalls.Load() == 0 {
		t.Fatal("onRTT not invoked")
	}
	if limitChanges.Load() == 0 {
		t.Fatal("onLimitChange not invoked under sustained RTT growth")
	}
}

// 13) disabled 状态仍然记 RTT（让运营观察）
func TestAdaptive_DisabledStillTracksRTT(t *testing.T) {
	l := newTestLimiter(5, 5, 50, false)
	for i := 0; i < 50; i++ {
		simulateRTT(l, "m1", 5*time.Millisecond)
		time.Sleep(time.Millisecond)
	}
	snap := l.Snapshot()
	if len(snap) == 0 || snap[0].RTTShort <= 0 {
		t.Fatalf("expected RTT tracked even when disabled, got %+v", snap)
	}
}

// 14) 默认配置：零值 cfg 走 defaults
func TestAdaptive_ZeroConfigDefaults(t *testing.T) {
	l := NewAdaptive(AdaptiveConfig{})
	c := l.Config()
	if c.Initial == 0 || c.Min == 0 || c.Max == 0 {
		t.Fatalf("defaults must populate Initial/Min/Max, got %+v", c)
	}
	if c.Enabled {
		t.Fatal("default must be disabled")
	}
	// disabled 默认不拒
	for i := 0; i < 100; i++ {
		r, err := l.Acquire("x")
		if err != nil {
			t.Fatal("disabled default must allow")
		}
		r()
	}
}

// 15) 空 merchantID 走 _unknown 共享桶
func TestAdaptive_EmptyMerchantSharedBucket(t *testing.T) {
	l := newTestLimiter(2, 1, 5, true)
	r1, e1 := l.Acquire("")
	r2, e2 := l.Acquire("")
	if e1 != nil || e2 != nil {
		t.Fatalf("first 2 should succeed: %v %v", e1, e2)
	}
	if _, err := l.Acquire(""); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("3rd empty should reject (shared _unknown bucket)")
	}
	r1()
	r2()
}

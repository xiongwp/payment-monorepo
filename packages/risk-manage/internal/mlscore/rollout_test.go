package mlscore

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestBucketFor_Stable 同一 customer_id + rolloutID 永远映射到同一 bucket。
// 保证灰度时同一用户不会"忽 champion 忽 challenger"。
func TestBucketFor_Stable(t *testing.T) {
	tests := []struct {
		cust, rid string
	}{
		{"cust_0001", "v2-1700000000000000000"},
		{"cust_abc", "rollout-x"},
		{"u_42", "r2"},
	}
	for _, tc := range tests {
		b1 := BucketFor(tc.cust, tc.rid)
		b2 := BucketFor(tc.cust, tc.rid)
		if b1 != b2 {
			t.Fatalf("bucket not stable for %q/%q: %d vs %d", tc.cust, tc.rid, b1, b2)
		}
		if b1 < 0 || b1 >= 100 {
			t.Fatalf("bucket out of range: %d", b1)
		}
	}
}

// TestBucketFor_EmptyCustomer 没有 customer_id 时返回 -1（caller fallback）。
func TestBucketFor_EmptyCustomer(t *testing.T) {
	if BucketFor("", "r") != -1 {
		t.Fatal("expected -1 for empty customer_id")
	}
}

// TestBucketFor_DifferentRolloutsDecorrelate 同一 customer 在不同 rollout
// 下落入的桶不应高度相关——避免"倒霉用户连续多个版本都是小白鼠"。
func TestBucketFor_DifferentRolloutsDecorrelate(t *testing.T) {
	var same int
	const N = 10_000
	for i := 0; i < N; i++ {
		cust := fmt.Sprintf("cust_%d", i)
		b1 := BucketFor(cust, "rollout-a")
		b2 := BucketFor(cust, "rollout-b")
		if b1 == b2 {
			same++
		}
	}
	// 独立 → 期望 ~1% 同桶；允许 0.5%-2%。
	if same < N/200 || same > N/50 {
		t.Errorf("buckets correlated across rollouts: same=%d / N=%d (expect ~%d)", same, N, N/100)
	}
}

// TestRollout_TrafficSplit_5pct 5% stage 下蒙特卡洛 10K 客户，验证大约 5% 落
// challenger，允许 ±1% 误差。同时验证同一 customer 多次评估走同一 model。
func TestRollout_TrafficSplit_5pct(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	if err := cc.StartRollout("v2", []Stage{{Pct: 5, MinObs: 0}, {Pct: 100, MinObs: 0}}, false, true); err != nil {
		t.Fatal(err)
	}

	const N = 10_000
	var challengerCount int
	results := make(map[string]float64) // customer → first score
	for i := 0; i < N; i++ {
		cust := fmt.Sprintf("cust_%d", i)
		r, err := cc.Score(context.Background(), Features{CustomerID: cust})
		if err != nil {
			t.Fatal(err)
		}
		if r.Score == chal.score {
			challengerCount++
		}
		results[cust] = r.Score
	}
	pct := float64(challengerCount) / float64(N) * 100
	if pct < 4 || pct > 6 {
		t.Errorf("expected ~5%% challenger traffic; got %.2f%% (count=%d / %d)", pct, challengerCount, N)
	}

	// 稳定性：每个 customer 重复评估 3 次，分数必须不变。
	for cust, want := range results {
		for k := 0; k < 3; k++ {
			r, _ := cc.Score(context.Background(), Features{CustomerID: cust})
			if r.Score != want {
				t.Fatalf("instability: cust=%s want=%v got=%v on attempt %d", cust, want, r.Score, k)
			}
		}
	}
}

// TestRollout_NoCustomerID_FallbackChampion 没 customer_id 时全部走 champion。
func TestRollout_NoCustomerID_FallbackChampion(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	if err := cc.StartRollout("v2", []Stage{{Pct: 50, MinObs: 0}, {Pct: 100, MinObs: 0}}, false, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		r, _ := cc.Score(context.Background(), Features{}) // 空 CustomerID
		if r.Score != champ.score {
			t.Fatalf("matchless request should fallback to champion; got %v", r.Score)
		}
	}
}

// TestRollout_Advance 推进到下一 stage 后流量比例提升。
func TestRollout_Advance(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	stages := []Stage{
		{Pct: 5, MinObs: 0},
		{Pct: 50, MinObs: 0},
		{Pct: 100, MinObs: 0},
	}
	if err := cc.StartRollout("v2", stages, false, true); err != nil {
		t.Fatal(err)
	}
	if got := cc.RolloutStatus().CurrentPct; got != 5 {
		t.Fatalf("initial stage pct should be 5; got %d", got)
	}
	if err := cc.AdvanceRollout(); err != nil {
		t.Fatal(err)
	}
	if got := cc.RolloutStatus().CurrentPct; got != 50 {
		t.Fatalf("after advance pct should be 50; got %d", got)
	}
	if err := cc.AdvanceRollout(); err != nil {
		t.Fatal(err)
	}
	// 推到 100% → rollout finalize → status RolloutID 应清空
	st := cc.RolloutStatus()
	if st.RolloutID != "" {
		t.Fatalf("after 100%% rollout should be finalized; got %+v", st)
	}
	if cc.ChampionName() != "v2" {
		t.Fatalf("after finalize champion should be v2; got %s", cc.ChampionName())
	}
}

// TestRollout_Rollback 手动 rollback 回退一个 stage。
func TestRollout_Rollback(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	stages := []Stage{
		{Pct: 5, MinObs: 0},
		{Pct: 50, MinObs: 0},
		{Pct: 100, MinObs: 0},
	}
	if err := cc.StartRollout("v2", stages, false, true); err != nil {
		t.Fatal(err)
	}
	_ = cc.AdvanceRollout() // 5 → 50
	if err := cc.RollbackRollout("smoke test"); err != nil {
		t.Fatal(err)
	}
	if got := cc.RolloutStatus().CurrentPct; got != 5 {
		t.Fatalf("after rollback pct should be 5; got %d", got)
	}
	if st := cc.RolloutStatus(); st.LastRollbackReason != "smoke test" {
		t.Errorf("expected reason recorded; got %q", st.LastRollbackReason)
	}
	// 已在 0 时 rollback 应报错
	if err := cc.RollbackRollout("again"); err != ErrRolloutAtZero {
		t.Errorf("expected ErrRolloutAtZero; got %v", err)
	}
}

// TestRollout_Pause 暂停后流量全部回 champion；Resume 恢复。
func TestRollout_Pause(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	if err := cc.StartRollout("v2", []Stage{{Pct: 50, MinObs: 0}, {Pct: 100, MinObs: 0}}, false, true); err != nil {
		t.Fatal(err)
	}
	if err := cc.PauseRollout(); err != nil {
		t.Fatal(err)
	}
	var challengerHits int
	for i := 0; i < 200; i++ {
		r, _ := cc.Score(context.Background(), Features{CustomerID: fmt.Sprintf("c%d", i)})
		if r.Score == chal.score {
			challengerHits++
		}
	}
	if challengerHits != 0 {
		t.Fatalf("paused rollout should send 0 traffic to challenger; got %d", challengerHits)
	}
	if err := cc.ResumeRollout(); err != nil {
		t.Fatal(err)
	}
	challengerHits = 0
	for i := 0; i < 200; i++ {
		r, _ := cc.Score(context.Background(), Features{CustomerID: fmt.Sprintf("c%d", i)})
		if r.Score == chal.score {
			challengerHits++
		}
	}
	if challengerHits == 0 {
		t.Fatal("resumed rollout should send some traffic to challenger")
	}
}

// mockMetricsProvider 让我们注入 challenger 表现数据驱动 auto-rollback。
type mockMetricsProvider struct {
	diff, lo, hi float64
	labeled      int
	ok           bool
}

func (m *mockMetricsProvider) ChallengerVsChampion(_ string) (float64, float64, float64, int, bool) {
	return m.diff, m.lo, m.hi, m.labeled, m.ok
}

// TestRollout_AutoRollback_TriggersWhenChallengerWorse mock challenger AUC 比
// champion 低 10%（CI 上界都 < 0）→ EvaluateAutoRollback 触发 rollback。
func TestRollout_AutoRollback_TriggersWhenChallengerWorse(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	stages := []Stage{
		{Pct: 5, MinObs: 100},
		{Pct: 50, MinObs: 100},
		{Pct: 100, MinObs: 0},
	}
	if err := cc.StartRollout("v2", stages, false, true); err != nil {
		t.Fatal(err)
	}
	_ = cc.AdvanceRollout() // 5 → 50；现在 currentStage=1

	mp := &mockMetricsProvider{
		diff:    -0.10,
		lo:      -0.15,
		hi:      -0.05, // CI 上界 < 0 → challenger 显著差
		labeled: 500,
		ok:      true,
	}
	rolled, reason := cc.EvaluateAutoRollback(mp)
	if !rolled {
		t.Fatalf("expected auto-rollback; reason: %s", reason)
	}
	if got := cc.RolloutStatus().CurrentPct; got != 5 {
		t.Fatalf("after auto-rollback pct should be 5; got %d", got)
	}
}

// TestRollout_AutoRollback_HoldsWhenInsufficient labeled < MinObs 时不下结论。
func TestRollout_AutoRollback_HoldsWhenInsufficient(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	stages := []Stage{
		{Pct: 5, MinObs: 10_000},
		{Pct: 100, MinObs: 0},
	}
	if err := cc.StartRollout("v2", stages, false, true); err != nil {
		t.Fatal(err)
	}
	// Challenger 看起来很烂，但只有 100 条 labeled——不到 MinObs
	mp := &mockMetricsProvider{diff: -0.2, lo: -0.3, hi: -0.1, labeled: 100, ok: true}
	rolled, reason := cc.EvaluateAutoRollback(mp)
	if rolled {
		t.Fatalf("should not rollback with insufficient samples; reason: %s", reason)
	}
}

// TestRollout_AutoRollback_HoldsWhenChallengerHealthy CI 上界 >= threshold 时
// 不应触发。
func TestRollout_AutoRollback_HoldsWhenChallengerHealthy(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	if err := cc.StartRollout("v2",
		[]Stage{{Pct: 5, MinObs: 100}, {Pct: 100, MinObs: 0}},
		false, true); err != nil {
		t.Fatal(err)
	}
	// challenger 在 CI 内含 0：不显著差也不显著好 → 不 rollback
	mp := &mockMetricsProvider{diff: 0.005, lo: -0.01, hi: 0.02, labeled: 500, ok: true}
	rolled, _ := cc.EvaluateAutoRollback(mp)
	if rolled {
		t.Fatal("should not rollback when challenger is healthy")
	}
}

// TestRollout_StartRequiresRegisteredChallenger 起 rollout 前 challenger 必须
// 已注册。
func TestRollout_StartRequiresRegisteredChallenger(t *testing.T) {
	cc := NewChampionChallenger("v1", &fixedSvc{name: "v1", score: 0})
	if err := cc.StartRollout("ghost", nil, false, true); err == nil {
		t.Fatal("expected error when challenger not registered")
	}
}

// TestRollout_StartTwiceFails 不允许并发 rollout。
func TestRollout_StartTwiceFails(t *testing.T) {
	cc := NewChampionChallenger("v1", &fixedSvc{name: "v1", score: 0})
	cc.RegisterChallenger("v2", &fixedSvc{name: "v2", score: 0.5})
	cc.RegisterChallenger("v3", &fixedSvc{name: "v3", score: 0.9})
	if err := cc.StartRollout("v2", nil, false, true); err != nil {
		t.Fatal(err)
	}
	if err := cc.StartRollout("v3", nil, false, true); err == nil {
		t.Fatal("expected error starting second rollout")
	}
}

// TestRollout_ReplaceChallenger_ResetsBuckets 换 challenger 后 rollout_id 变
// 化，bucket 分布重新洗牌。
func TestRollout_ReplaceChallenger_ResetsBuckets(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	v2 := &fixedSvc{name: "v2", score: 0.80}
	v2_1 := &fixedSvc{name: "v2.1", score: 0.85}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", v2)
	if err := cc.StartRollout("v2", []Stage{{Pct: 50, MinObs: 0}, {Pct: 100, MinObs: 0}}, false, true); err != nil {
		t.Fatal(err)
	}
	ridBefore := cc.RolloutStatus().RolloutID
	if err := cc.ReplaceChallenger("v2.1", v2_1); err != nil {
		t.Fatal(err)
	}
	ridAfter := cc.RolloutStatus().RolloutID
	if ridBefore == ridAfter {
		t.Fatal("rollout_id should change after ReplaceChallenger")
	}
	if cc.RolloutStatus().ChallengerName != "v2.1" {
		t.Fatalf("expected challenger v2.1; got %s", cc.RolloutStatus().ChallengerName)
	}
	if cc.RolloutStatus().CurrentStage != 0 {
		t.Fatal("ReplaceChallenger should reset stage to 0")
	}
	// 实际跑：v2.1 应被路由到（pct=50）
	var v21Hits int
	for i := 0; i < 1000; i++ {
		r, _ := cc.Score(context.Background(), Features{CustomerID: fmt.Sprintf("u%d", i)})
		if r.Score == v2_1.score {
			v21Hits++
		}
	}
	if v21Hits < 400 || v21Hits > 600 {
		t.Errorf("expected ~500 hits to v2.1 at 50%% stage; got %d", v21Hits)
	}
}

// TestRollout_PromoteShortcutFinalizesRollout 老的 PromoteChallenger 接口对
// 正在 rollout 的 challenger 等价于"直接推到 100%"。
func TestRollout_PromoteShortcutFinalizesRollout(t *testing.T) {
	cc := NewChampionChallenger("v1", &fixedSvc{name: "v1", score: 0.1})
	cc.RegisterChallenger("v2", &fixedSvc{name: "v2", score: 0.9})
	if err := cc.StartRollout("v2", []Stage{{Pct: 5, MinObs: 0}, {Pct: 100, MinObs: 0}}, false, true); err != nil {
		t.Fatal(err)
	}
	if !cc.PromoteChallenger("v2") {
		t.Fatal("PromoteChallenger should succeed")
	}
	if cc.ChampionName() != "v2" {
		t.Fatal("champion should be v2")
	}
	if cc.RolloutStatus().RolloutID != "" {
		t.Fatal("rollout should be cleared after promote shortcut")
	}
}

// TestRollout_SideEffectGetsChampionScoreEvenWhenRoutedToChallenger
// rollout 路由到 challenger 时，SideEffect 收到的 championResult 仍是 champion
// 视角分数（额外补跑），让 ABTracker 能算 AUC 差。
func TestRollout_SideEffectGetsChampionScoreEvenWhenRoutedToChallenger(t *testing.T) {
	champ := &fixedSvc{name: "v1", score: 0.10}
	chal := &fixedSvc{name: "v2", score: 0.80}
	cc := NewChampionChallenger("v1", champ)
	cc.RegisterChallenger("v2", chal)
	if err := cc.StartRollout("v2", []Stage{{Pct: 100, MinObs: 0}}, false, true); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var gotChampion Result
	var gotChallengers []NamedResult
	cc.SideEffect = func(_ string, ch Result, ngs []NamedResult) {
		mu.Lock()
		gotChampion = ch
		gotChallengers = append([]NamedResult(nil), ngs...)
		mu.Unlock()
	}
	r, _ := cc.Score(context.Background(), Features{CustomerID: "cust_x"})
	if r.Score != chal.score {
		t.Fatalf("expected primary path to be challenger; got %v", r.Score)
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if gotChampion.Score != champ.score {
		t.Errorf("SideEffect championResult should be champion's score %v; got %v", champ.score, gotChampion.Score)
	}
	var sawChallenger bool
	for _, n := range gotChallengers {
		if n.Name == "v2" && n.Score == chal.score {
			sawChallenger = true
		}
	}
	if !sawChallenger {
		t.Errorf("SideEffect should include challenger v2 result; got %+v", gotChallengers)
	}
}

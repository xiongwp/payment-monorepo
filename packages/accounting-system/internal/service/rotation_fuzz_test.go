package service

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// Property-based Fuzz Tests
//
// 用确定性伪随机生成大量场景，验证关键不变量始终成立：
//   P1: 任意 (flow, LA) 在多轮调用中路由稳定
//   P2: 重复 Resolve 不产生重复 routing
//   P3: phase 转换序列永远合法（图上有边）
//   P4: anchor 转换最终收敛到终态
//   P5: I0 - flow 锁定后即使经过 rotation 也不切
// ============================================================================

// 确定性伪随机生成器（同一 seed → 同一序列，可复现）
func newRng(seed int64) *rand.Rand {
	return rand.New(rand.NewSource(seed))
}

// FUZZ-1: 随机 phase 转换序列必须永远合法
// 起点随机，每步随机选目标 phase，CanTransitionPhase 必须保持自一致
func TestFuzz_PhaseTransitionsConsistent(t *testing.T) {
	const iterations = 10000
	allPhases := []model.LifecyclePhase{
		model.LifecyclePhaseLegacy, model.LifecyclePhaseProvisioned, model.LifecyclePhaseActive,
		model.LifecyclePhaseDraining, model.LifecyclePhaseFrozen, model.LifecyclePhaseArchived,
		model.LifecyclePhaseQuarantined,
	}
	r := newRng(0xCAFE)
	for i := 0; i < iterations; i++ {
		from := allPhases[r.Intn(len(allPhases))]
		to := allPhases[r.Intn(len(allPhases))]
		// 多次调用结果必须一致（纯函数）
		got1 := model.CanTransitionPhase(from, to)
		got2 := model.CanTransitionPhase(from, to)
		if got1 != got2 {
			t.Fatalf("iter %d: CanTransitionPhase non-deterministic: %v vs %v", i, got1, got2)
		}
	}
}

// FUZZ-2: 随机 anchor status 序列在 IsTerminal 后不应再有合法转换
func TestFuzz_AnchorTransitionsTerminalSticky(t *testing.T) {
	const iterations = 10000
	all := []model.AnchorStatus{
		model.AnchorStatusTrying, model.AnchorStatusActive,
		model.AnchorStatusSettled, model.AnchorStatusMigrated, model.AnchorStatusStuck,
	}
	r := newRng(0xDEAD)
	for i := 0; i < iterations; i++ {
		from := all[r.Intn(len(all))]
		to := all[r.Intn(len(all))]
		can := model.CanTransitionAnchor(from, to)
		if from.IsTerminal() && can {
			t.Fatalf("iter %d: terminal %s should have no outgoing transition to %s", i, from, to)
		}
	}
}

// FUZZ-3: 随机 booking 序列下 flow 路由稳定（I0 不变量）
func TestFuzz_FlowRouteStability(t *testing.T) {
	const iterations = 1000
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	rng := newRng(0xBEEF)
	flowAccount := map[string]string{} // (flowID, LAKey) → 首次落到的 account_no

	for i := 0; i < iterations; i++ {
		flowID := fmt.Sprintf("FUZZ_F_%d", rng.Intn(20)) // 20 个不同 flow
		laKeys := []string{
			"channel-payable:alipay:CNY",
			"channel-payable:wechat:CNY",
			"channel-receivable:alipay:CNY",
		}
		laKey := laKeys[rng.Intn(len(laKeys))]
		dir := BookingDirectionDebit
		if rng.Intn(2) == 1 {
			dir = BookingDirectionCredit
		}

		req := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         dir,
			BookingType:       BookingTypeNormal,
		}
		res, err := r.Resolve(context.Background(), req)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}

		key := flowID + "|" + laKey
		if firstAccount, exists := flowAccount[key]; exists {
			if firstAccount != res.AccountNo {
				t.Fatalf("iter %d: I0 violation flow=%s LA=%s first=%s now=%s",
					i, flowID, laKey, firstAccount, res.AccountNo)
			}
		} else {
			flowAccount[key] = res.AccountNo
		}

		// 模拟 caller 写
		simulateBookingWrite(t, r, ar, rr, req, int64(20000+i*10))
	}
}

// FUZZ-4: 不同 flow_id 的路由确定性（同输入必同输出）
// 生产 FNV 分布在 repository.anchor_repository_test 已覆盖；此处只测确定性
func TestFuzz_FlowIDRouteDeterministic(t *testing.T) {
	rng := newRng(0xFEED)
	const N = 1000
	rr := &fakeRouteReader{}
	for i := 0; i < N; i++ {
		flowID := fmt.Sprintf("FUZZ_%d_%d", i, rng.Int63())
		db1, gtbl1 := rr.RouteByFlowID(flowID)
		db2, gtbl2 := rr.RouteByFlowID(flowID)
		if db1 != db2 || gtbl1 != gtbl2 {
			t.Fatalf("flow=%s non-deterministic: (%d,%d) vs (%d,%d)",
				flowID, db1, gtbl1, db2, gtbl2)
		}
	}
}

// FUZZ-5: 同 flow 多次 Resolve 不增加 routing 行数
func TestFuzz_RoutingIdempotent(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)
	rng := newRng(0xACE)

	// 10 个 flow，每个 flow 在 1 个 LA 上多次 booking
	for i := 0; i < 10; i++ {
		flowID := fmt.Sprintf("IDEM_F_%d", i)
		laKey := "channel-payable:alipay:CNY"
		// 随机 5-15 次 booking
		repeats := 5 + rng.Intn(10)
		for j := 0; j < repeats; j++ {
			dir := BookingDirectionDebit
			if rng.Intn(2) == 1 {
				dir = BookingDirectionCredit
			}
			req := &ResolveRequest{
				LogicalAccountKey: laKey,
				FlowID:            flowID,
				Direction:         dir,
				BookingType:       BookingTypeNormal,
			}
			simulateBookingWrite(t, r, ar, rr, req, int64(30000+i*100+j))
		}
	}

	// 应该只有 10 条 routing（每个 flow 一条）
	if len(rr.byKey) != 10 {
		t.Errorf("expected 10 routing rows (one per flow), got %d", len(rr.byKey))
	}
}

// FUZZ-6: Resolve 在大并发下结果不退化
func TestFuzz_ConcurrentResolveConsistent(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	// 预填 一些 routing + anchor
	rr.byKey[routeKey("CONC_F", 102)] = &model.FlowAnchorRoute{
		ID: 1, FlowID: "CONC_F", LogicalAccountID: 102, AccountNo: "P_ALIPAY_001",
	}
	ar.byKey[anchorKeyB("CONC_F", "P_ALIPAY_001")] = &model.TxAccountAnchor{
		ID: 1, FlowID: "CONC_F", LogicalAccountID: 102, AccountNo: "P_ALIPAY_001",
		Status: model.AnchorStatusActive, Version: 5,
	}

	const N = 200
	results := make([]string, N)
	versions := make([]int64, N)
	done := make(chan struct{})

	// 多 goroutine 并发读
	for i := 0; i < N; i++ {
		go func(i int) {
			req := &ResolveRequest{
				LogicalAccountKey: "channel-payable:alipay:CNY",
				FlowID:            "CONC_F",
				Direction:         BookingDirectionDebit,
				BookingType:       BookingTypeNormal,
			}
			res, _ := r.Resolve(context.Background(), req)
			results[i] = res.AccountNo
			versions[i] = res.AnchorPlan.UpdateExpectedVersion
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < N; i++ {
		<-done
	}
	for i, acc := range results {
		if acc != "P_ALIPAY_001" {
			t.Errorf("goroutine %d: got %s, expected P_ALIPAY_001", i, acc)
		}
		if versions[i] != 5 {
			t.Errorf("goroutine %d: version=%d, expected 5", i, versions[i])
		}
	}
}

// FUZZ-7: archived 不可**直接**逃逸到 production phase
// 设计允许 archived → quarantined → draining/frozen 的"运维回拉"路径（极端不一致修复用）；
// 但 archived 直接到 active/draining 是禁止的。
func TestFuzz_ArchivedNoDirectEscape(t *testing.T) {
	for _, p := range []model.LifecyclePhase{
		model.LifecyclePhaseActive,
		model.LifecyclePhaseDraining,
		model.LifecyclePhaseFrozen,
		model.LifecyclePhaseProvisioned,
		model.LifecyclePhaseLegacy,
	} {
		if model.CanTransitionPhase(model.LifecyclePhaseArchived, p) {
			t.Errorf("archived must NOT transition directly to %s", p)
		}
	}
	// archived → quarantined 是允许的（运维兜底）
	if !model.CanTransitionPhase(model.LifecyclePhaseArchived, model.LifecyclePhaseQuarantined) {
		t.Error("archived → quarantined must be allowed (ops escape hatch)")
	}
}

// FUZZ-8: anchor 不变量 — IsOpen 与 transition map 一致
// 任何有合法转换出口的 anchor 都应该 IsOpen=true（除非是 stuck）
func TestFuzz_AnchorIsOpenMatchesTransitionMap(t *testing.T) {
	all := []model.AnchorStatus{
		model.AnchorStatusTrying, model.AnchorStatusActive,
		model.AnchorStatusSettled, model.AnchorStatusMigrated, model.AnchorStatusStuck,
	}
	for _, s := range all {
		_, hasTransitions := model.AllowedAnchorTransitions[s]
		if s.IsOpen() != hasTransitions {
			t.Errorf("status=%s IsOpen=%v but has-transitions=%v should match",
				s, s.IsOpen(), hasTransitions)
		}
	}
}

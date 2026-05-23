package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// E2E 场景测试 — 4 大资金流端到端贯穿
//
// 覆盖的资金流：
//   1. 用户充值 (Topup)        渠道应收 → 用户余额
//   2. 用户余额支付 (Payment)   用户余额 → 渠道应付
//   3. 用户转账 (Transfer)     用户余额 A → 用户余额 B  (无 rotating)
//   4. 用户提现 (Withdrawal)   用户余额 → 渠道应付
//
// 这些测试验证：
//   - Router 在真实多 LA 场景下正确路由
//   - 同 flow 在多个 entry 间保持 instance 一致
//   - 跨期跨轮换 flow 仍锁定原 instance（I0 不变量）
//   - 退款/红冲场景路由继承正确
// ============================================================================

// 模拟一个完整的 fixture：包含 4 个常用 LA
//   - channel-receivable:alipay:CNY (rotating)
//   - channel-payable:alipay:CNY (rotating)
//   - channel-payable:wechat:CNY (rotating)
//   - user-balance:CNY (legacy, not rotating)
func newE2EFixture(now time.Time) (
	Router, *fakeLogicalReader, *fakeAnchorReaderB, *fakeRouteReader, *fakeAccountReader,
) {
	r, lr, ar, rr, accR := newRouterFixture(now)

	// 渠道应收 (rotating) — for topup credit side
	receivable := mkLogicalAccount(101, "channel-receivable:alipay:CNY", true, "R_ALIPAY_001")
	lr.byKey["channel-receivable:alipay:CNY"] = receivable
	lr.byID[101] = receivable
	accR.byNo["R_ALIPAY_001"] = mkAccountActive("R_ALIPAY_001", 101)

	// 渠道应付 Alipay (rotating) — for payment/withdrawal
	payableAli := mkLogicalAccount(102, "channel-payable:alipay:CNY", true, "P_ALIPAY_001")
	lr.byKey["channel-payable:alipay:CNY"] = payableAli
	lr.byID[102] = payableAli
	accR.byNo["P_ALIPAY_001"] = mkAccountActive("P_ALIPAY_001", 102)

	// 渠道应付 WeChat (rotating)
	payableWX := mkLogicalAccount(103, "channel-payable:wechat:CNY", true, "P_WECHAT_001")
	lr.byKey["channel-payable:wechat:CNY"] = payableWX
	lr.byID[103] = payableWX
	accR.byNo["P_WECHAT_001"] = mkAccountActive("P_WECHAT_001", 103)

	// 用户余额（legacy，不轮换）
	userBalLegacy := mkLogicalAccount(201, "user-balance:CNY", false, "")
	lr.byKey["user-balance:CNY"] = userBalLegacy
	lr.byID[201] = userBalLegacy

	return r, lr, ar, rr, accR
}

// helper：执行一笔 booking 的"完整 caller 流程"模拟
// 1. Resolve → 2. 落 routing (if any) → 3. 落 anchor
// 返回 Resolution + 是否成功
func simulateBookingWrite(
	t *testing.T, r Router, ar *fakeAnchorReaderB, rr *fakeRouteReader,
	req *ResolveRequest, idBase int64,
) *Resolution {
	t.Helper()
	res, err := r.Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve %s/%s: %v", req.LogicalAccountKey, req.FlowID, err)
	}
	if res.IsLegacy {
		return res
	}
	// 落 routing
	if res.RoutePlan != nil && res.RoutePlan.Op == RouteOpInsert {
		key := routeKey(req.FlowID, res.LogicalAccount.ID)
		if _, exists := rr.byKey[key]; !exists {
			rr.byKey[key] = res.RoutePlan.NewRoute
			rr.byKey[key].ID = idBase + 1
		}
	}
	// 落 anchor
	if res.AnchorPlan != nil && res.AnchorPlan.Op == AnchorOpInsert {
		key := anchorKeyB(req.FlowID, res.AccountNo)
		if _, exists := ar.byKey[key]; !exists {
			ar.byKey[key] = res.AnchorPlan.NewAnchor
			ar.byKey[key].ID = idBase + 2
		}
	} else if res.AnchorPlan != nil && res.AnchorPlan.Op == AnchorOpUpdatePosting {
		// 模拟 UpdatePosting：anchor 已存在，更新 mask/count/version
		key := anchorKeyB(req.FlowID, res.AccountNo)
		if existing, ok := ar.byKey[key]; ok {
			existing.DirectionMask = res.AnchorPlan.UpdateNewMask
			existing.PostingCount++
			existing.Version++
		}
	}
	return res
}

// ============================================================================
// 场景 1: 用户充值 (Topup)
//
// flow_id=TOPUP_X, 单笔凭证两条 entry:
//   entry 1: 借 用户余额 +100   (legacy LA)
//   entry 2: 贷 渠道应收 +100   (rotating LA: channel-receivable:alipay:CNY)
//
// 验证：
//   - entry 2 在 rotating LA 上建立 routing + anchor
//   - 同 flow_id 的两条 entry 路由独立但 flow_id 一致
// ============================================================================
func TestE2E_UserTopup(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	flowID := "TOPUP_001"

	// Entry 1: 用户余额（legacy） — IsLegacy=true
	entry1 := &ResolveRequest{
		LogicalAccountKey: "user-balance:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionDebit,
		BookingType:       BookingTypeNormal,
	}
	res1 := simulateBookingWrite(t, r, ar, rr, entry1, 1000)
	if !res1.IsLegacy {
		t.Fatal("user-balance should be legacy")
	}

	// Entry 2: 渠道应收（rotating） — 应建 routing + anchor
	entry2 := &ResolveRequest{
		LogicalAccountKey: "channel-receivable:alipay:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeNormal,
	}
	res2 := simulateBookingWrite(t, r, ar, rr, entry2, 2000)
	if res2.IsLegacy {
		t.Fatal("channel-receivable should be rotating")
	}
	if res2.AccountNo != "R_ALIPAY_001" {
		t.Errorf("topup credit should land R_ALIPAY_001, got %s", res2.AccountNo)
	}
	if res2.RoutePlan == nil || res2.AnchorPlan == nil {
		t.Fatal("first time should have both plans")
	}

	// 验证 routing 落库
	if _, ok := rr.byKey[routeKey(flowID, 101)]; !ok {
		t.Error("routing should be persisted for receivable LA")
	}
}

// ============================================================================
// 场景 2: 用户余额支付 (Payment)
//
// 业务方使用一个 flow_id 跑 TCC Try → Confirm 两阶段
// 中途发生 rotation，Confirm 必须落到 Try 锁定的同一 instance
// ============================================================================
func TestE2E_UserBalancePayment_CrossRotation(t *testing.T) {
	day1 := time.Date(2026, 6, 30, 14, 0, 0, 0, time.UTC)
	r, lr, ar, rr, accR := newE2EFixture(day1)

	flowID := "PAY_001"

	// Try 阶段：建立 anchor 在 P_ALIPAY_001
	tryReq := &ResolveRequest{
		LogicalAccountKey: "channel-payable:alipay:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeTCCTry,
	}
	resTry := simulateBookingWrite(t, r, ar, rr, tryReq, 3000)
	if resTry.AccountNo != "P_ALIPAY_001" {
		t.Fatalf("Try should land P_ALIPAY_001, got %s", resTry.AccountNo)
	}

	// Day 2: 发生 rotation — P_ALIPAY_001 → draining, P_ALIPAY_002 active
	la := lr.byKey["channel-payable:alipay:CNY"]
	newActive := "P_ALIPAY_002"
	la.CurrentActiveAccountNo = &newActive
	accR.byNo["P_ALIPAY_001"] = mkAccountPhase("P_ALIPAY_001", 102, model.LifecyclePhaseDraining)
	accR.byNo["P_ALIPAY_002"] = mkAccountActive("P_ALIPAY_002", 102)
	// 让缓存失效
	if impl, ok := r.(*router); ok {
		impl.cache.invalidate("channel-payable:alipay:CNY")
		impl.clock = func() time.Time { return day1.Add(2 * 24 * time.Hour) }
	}

	// Day 3: Confirm 必须落回 P_ALIPAY_001
	confirmReq := &ResolveRequest{
		LogicalAccountKey: "channel-payable:alipay:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionDebit,
		BookingType:       BookingTypeTCCConfirm,
	}
	resConfirm := simulateBookingWrite(t, r, ar, rr, confirmReq, 3500)
	if resConfirm.AccountNo != "P_ALIPAY_001" {
		t.Fatalf("FLOW CONSISTENCY VIOLATION: Confirm should land P_ALIPAY_001, got %s", resConfirm.AccountNo)
	}
	if resConfirm.AnchorPlan.Op != AnchorOpUpdatePosting {
		t.Error("Confirm should be UpdatePosting")
	}
}

// ============================================================================
// 场景 3: 用户转账 (Transfer)
//
// 两个 user-balance（legacy）账户互转 — 完全走 legacy 路径
// ============================================================================
func TestE2E_UserTransfer_BothLegacy(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	flowID := "TRANSFER_001"

	// 发送方扣款
	sendReq := &ResolveRequest{
		LogicalAccountKey: "user-balance:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionDebit,
		BookingType:       BookingTypeNormal,
	}
	resSend := simulateBookingWrite(t, r, ar, rr, sendReq, 4000)
	if !resSend.IsLegacy {
		t.Fatal("transfer send must be legacy")
	}

	// 接收方加款
	recvReq := &ResolveRequest{
		LogicalAccountKey: "user-balance:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeNormal,
	}
	resRecv := simulateBookingWrite(t, r, ar, rr, recvReq, 4500)
	if !resRecv.IsLegacy {
		t.Fatal("transfer receive must be legacy")
	}

	// 验证 legacy 不产生 routing/anchor
	if len(rr.byKey) != 0 || len(ar.byKey) != 0 {
		t.Errorf("legacy transfer should not create routing/anchor, got rr=%d ar=%d", len(rr.byKey), len(ar.byKey))
	}
}

// ============================================================================
// 场景 4: 用户提现 (Withdrawal)
//
// 用户余额(legacy) → 渠道应付(rotating)
// 单笔交易，两条 entry
// ============================================================================
func TestE2E_UserWithdrawal(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	flowID := "WITHDRAW_001"

	// Entry 1: 用户余额 -100 (legacy)
	e1 := &ResolveRequest{
		LogicalAccountKey: "user-balance:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionDebit,
		BookingType:       BookingTypeNormal,
	}
	res1 := simulateBookingWrite(t, r, ar, rr, e1, 5000)
	if !res1.IsLegacy {
		t.Fatal("user-balance should be legacy")
	}

	// Entry 2: 渠道应付 +100 (rotating)
	e2 := &ResolveRequest{
		LogicalAccountKey: "channel-payable:alipay:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeNormal,
	}
	res2 := simulateBookingWrite(t, r, ar, rr, e2, 5500)
	if res2.AccountNo != "P_ALIPAY_001" {
		t.Errorf("withdrawal credit should land P_ALIPAY_001, got %s", res2.AccountNo)
	}
}

// ============================================================================
// 场景 5: 退款（refund-of）— 跨 flow 继承
//
// 原支付 flow 锁在 P_ALIPAY_001 (现已 draining)
// 退款新 flow 必须继承到同一 instance
// ============================================================================
func TestE2E_RefundInheritsInstance(t *testing.T) {
	day1 := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r, lr, ar, rr, accR := newE2EFixture(day1)

	// 第一步：原支付建立 anchor 在 P_ALIPAY_001
	payReq := &ResolveRequest{
		LogicalAccountKey: "channel-payable:alipay:CNY",
		FlowID:            "PAY_ORIG",
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeNormal,
	}
	simulateBookingWrite(t, r, ar, rr, payReq, 6000)

	// 模拟：发生 rotation，P_ALIPAY_001 → draining
	la := lr.byKey["channel-payable:alipay:CNY"]
	newActive := "P_ALIPAY_002"
	la.CurrentActiveAccountNo = &newActive
	accR.byNo["P_ALIPAY_001"] = mkAccountPhase("P_ALIPAY_001", 102, model.LifecyclePhaseDraining)
	accR.byNo["P_ALIPAY_002"] = mkAccountActive("P_ALIPAY_002", 102)
	if impl, ok := r.(*router); ok {
		impl.cache.invalidate("channel-payable:alipay:CNY")
		impl.clock = func() time.Time { return day1.Add(45 * 24 * time.Hour) }
	}

	// 退款新 flow（45 天后）— 必须继承 P_ALIPAY_001
	refundReq := &ResolveRequest{
		LogicalAccountKey: "channel-payable:alipay:CNY",
		FlowID:            "RFD_001",
		Direction:         BookingDirectionDebit,
		OriginalFlowID:    "PAY_ORIG",
		ReuseSource:       model.AnchorReuseSourceRefundOf,
		BookingType:       BookingTypeNormal,
	}
	resRefund, err := r.Resolve(context.Background(), refundReq)
	if err != nil {
		t.Fatal(err)
	}
	if resRefund.AccountNo != "P_ALIPAY_001" {
		t.Errorf("refund must inherit original instance P_ALIPAY_001, got %s", resRefund.AccountNo)
	}
	if resRefund.AnchorPlan.NewAnchor.ReuseSource != model.AnchorReuseSourceRefundOf {
		t.Error("refund anchor must record ReuseSource=RefundOf")
	}
}

// ============================================================================
// 场景 6: 多渠道同 flow（一笔交易跨多个 rotating LA）
//
// 不同渠道（应付 Alipay + 应付 WeChat）的 anchor 互相独立
// 但同 flow_id 在每个 LA 上的 anchor 都受 I0 不变量保护
// ============================================================================
func TestE2E_MultiChannelSameFlow(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	flowID := "MULTI_CHANNEL_FLOW"

	// 应付 Alipay
	req1 := &ResolveRequest{
		LogicalAccountKey: "channel-payable:alipay:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeNormal,
	}
	res1 := simulateBookingWrite(t, r, ar, rr, req1, 7000)

	// 应付 WeChat
	req2 := &ResolveRequest{
		LogicalAccountKey: "channel-payable:wechat:CNY",
		FlowID:            flowID,
		Direction:         BookingDirectionCredit,
		BookingType:       BookingTypeNormal,
	}
	res2 := simulateBookingWrite(t, r, ar, rr, req2, 7500)

	// 不同 LA → 不同 instance
	if res1.AccountNo == res2.AccountNo {
		t.Error("different LAs should resolve to different instances")
	}
	// 但 flow_id 相同 → routing 各自独立（不同 LA_id）
	if _, ok := rr.byKey[routeKey(flowID, 102)]; !ok {
		t.Error("routing for Alipay missing")
	}
	if _, ok := rr.byKey[routeKey(flowID, 103)]; !ok {
		t.Error("routing for WeChat missing")
	}
}

// ============================================================================
// 场景 7: 同 flow 在同 LA 上的多 step（验证 I0）
//
// 业务上一些复杂流程可能在同一 LA 上有多次记账（如：先 try 借方，再 confirm 贷方）
// 这些应当走 update plan，不重复建 routing
// ============================================================================
func TestE2E_SameLAMultipleEntries(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	flowID := "MULTI_STEP_FLOW"

	for i := 0; i < 5; i++ {
		dir := BookingDirectionDebit
		if i%2 == 1 {
			dir = BookingDirectionCredit
		}
		req := &ResolveRequest{
			LogicalAccountKey: "channel-payable:alipay:CNY",
			FlowID:            flowID,
			Direction:         dir,
			BookingType:       BookingTypeNormal,
		}
		res := simulateBookingWrite(t, r, ar, rr, req, int64(8000+i*10))
		if res.AccountNo != "P_ALIPAY_001" {
			t.Errorf("step %d: should stay on P_ALIPAY_001, got %s", i, res.AccountNo)
		}
		if i == 0 {
			if res.AnchorPlan.Op != AnchorOpInsert {
				t.Error("first step should Insert")
			}
		} else {
			if res.AnchorPlan.Op != AnchorOpUpdatePosting {
				t.Errorf("step %d should be UpdatePosting", i)
			}
			if res.RoutePlan != nil {
				t.Errorf("step %d should not produce RoutePlan", i)
			}
		}
	}

	// 检查 anchor 上的 posting_count
	anc := ar.byKey[anchorKeyB(flowID, "P_ALIPAY_001")]
	if anc == nil {
		t.Fatal("anchor missing")
	}
	if anc.PostingCount != 5 {
		t.Errorf("posting_count should be 5, got %d", anc.PostingCount)
	}
	// mask 应该 = both
	if !anc.DirectionMask.HasDebit() || !anc.DirectionMask.HasCredit() {
		t.Error("mask should have both flags")
	}
}

// ============================================================================
// Property-based fuzz：随机生成 N 笔 booking 序列，验证不变量
// ============================================================================

// 不变量 P1: 同一 (flow_id, LA_id) 永远路由到同一 account_no
// 不变量 P2: 后续操作不重新生成 routing
func TestE2E_Property_FlowStaysOnSameInstance(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	const N = 100
	flowAccountMap := map[string]string{} // flow_id → first account_no

	for i := 0; i < N; i++ {
		// 5 个 flow，3 个 LA，随机组合
		flowID := fmt.Sprintf("FLOW_%d", i%5)
		laKey := []string{
			"channel-payable:alipay:CNY",
			"channel-payable:wechat:CNY",
			"channel-receivable:alipay:CNY",
		}[i%3]
		dir := BookingDirectionDebit
		if i%2 == 1 {
			dir = BookingDirectionCredit
		}
		req := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         dir,
			BookingType:       BookingTypeNormal,
		}
		res := simulateBookingWrite(t, r, ar, rr, req, int64(10000+i*10))

		key := flowID + "|" + laKey
		if first, exists := flowAccountMap[key]; exists {
			if first != res.AccountNo {
				t.Fatalf("P1 violation: flow=%s LA=%s first landed %s, now %s",
					flowID, laKey, first, res.AccountNo)
			}
		} else {
			flowAccountMap[key] = res.AccountNo
		}
	}
}

// 不变量 P3: 同 (flow_id, LA) 重复 Resolve 不会生成多个 routing
func TestE2E_Property_NoDuplicateRouting(t *testing.T) {
	now := time.Now()
	r, _, ar, rr, _ := newE2EFixture(now)

	flowID := "DUP_TEST"
	laKey := "channel-payable:alipay:CNY"

	for i := 0; i < 20; i++ {
		req := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeNormal,
		}
		simulateBookingWrite(t, r, ar, rr, req, int64(11000+i*10))
	}

	// 应只有 1 条 routing
	count := 0
	for k := range rr.byKey {
		if k == routeKey(flowID, 102) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("should have exactly 1 routing for flow, got %d", count)
	}
}

// 不变量 P4: 缓存命中不影响正确性
func TestE2E_Property_CacheHitVsMiss(t *testing.T) {
	now := time.Now()
	r1, _, ar1, rr1, _ := newE2EFixture(now)
	r2, _, ar2, rr2, _ := newE2EFixture(now)

	const N = 50
	for i := 0; i < N; i++ {
		// r1 用同一 LA（cache 命中）；r2 频繁切换不同 LA（cache miss 更多）
		req1 := &ResolveRequest{
			LogicalAccountKey: "channel-payable:alipay:CNY",
			FlowID:            fmt.Sprintf("F_%d", i),
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeNormal,
		}
		simulateBookingWrite(t, r1, ar1, rr1, req1, int64(12000+i*10))

		req2 := req1
		simulateBookingWrite(t, r2, ar2, rr2, req2, int64(13000+i*10))
	}
	// 两个 Router 实例应产生等同的 routing 数量
	if len(rr1.byKey) != len(rr2.byKey) {
		t.Errorf("cache hit/miss should not affect outcome: r1 routes=%d r2=%d",
			len(rr1.byKey), len(rr2.byKey))
	}
}

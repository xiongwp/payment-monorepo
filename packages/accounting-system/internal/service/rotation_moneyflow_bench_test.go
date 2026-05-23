package service

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiongwp/accounting-system/internal/domain/model"
)

// ============================================================================
// 4 大资金流 TPS 压测
//
// 测量 Router 在生产规模 LA + 真实业务流模式下的纯逻辑吞吐：
//   1. 用户充值 (Topup)        渠道应收(rotating) ← 用户余额(legacy)
//   2. 用户余额支付 (Payment)   用户余额(legacy) → 渠道应付(rotating)
//   3. 用户转账 (Transfer)     用户余额(legacy) ↔ 用户余额(legacy)
//   4. 用户提现 (Withdrawal)   用户余额(legacy) → 渠道应付(rotating)
//
// **关键说明**：
// 这些 bench 用 in-memory fake repo（无 DB IO），测得是 **Router 路由逻辑天花板**。
// 真实生产 TPS 上限受限于：
//   - MySQL 写入 IOPS：每笔 booking 写 routing + anchor + transaction ≈ 3 行
//   - 网络往返：应用 ↔ MySQL
//   - uk_flow_account / uk_flow_logical 锁竞争
//
// 用法：
//   go test -bench=BenchmarkMoneyFlow -benchmem -benchtime=5s ./internal/service/
//
// 解读：
//   - ns/op：单次 Resolve 平均耗时
//   - TPS = 1e9 / ns/op (单核)
//   - 并行 TPS ≈ ns/op_parallel × GOMAXPROCS
// ============================================================================

// 生产规模 fixture：模拟 200 个 LA（涵盖多渠道 × 多币种），200 万用户余额账户
func buildMoneyFlowFixture() (
	Router, *fakeLogicalReader, *fakeAnchorReaderB, *fakeRouteReader, *fakeAccountReader,
) {
	lr := &fakeLogicalReader{
		byKey: make(map[string]*model.LogicalAccount, 400),
		byID:  make(map[int64]*model.LogicalAccount, 400),
	}
	ar := &fakeAnchorReaderB{byKey: make(map[string]*model.TxAccountAnchor, 1000000)}
	rr := &fakeRouteReader{byKey: make(map[string]*model.FlowAnchorRoute, 1000000)}
	accR := &fakeAccountReader{byNo: make(map[string]*model.Account, 10000)}

	now := time.Now()

	// 注册 100 个渠道 × 应付/应收 = 200 个 rotating LA
	for ch := 1; ch <= 100; ch++ {
		// 应付
		payID := int64(1000 + ch)
		payKey := fmt.Sprintf("channel-payable:ch%d:CNY", ch)
		payActive := fmt.Sprintf("P_CH%d_001", ch)
		la := mkLogicalAccount(payID, payKey, true, payActive)
		lr.byKey[payKey] = la
		lr.byID[payID] = la
		accR.byNo[payActive] = mkAccountActive(payActive, payID)

		// 应收
		recvID := int64(2000 + ch)
		recvKey := fmt.Sprintf("channel-receivable:ch%d:CNY", ch)
		recvActive := fmt.Sprintf("R_CH%d_001", ch)
		la2 := mkLogicalAccount(recvID, recvKey, true, recvActive)
		lr.byKey[recvKey] = la2
		lr.byID[recvID] = la2
		accR.byNo[recvActive] = mkAccountActive(recvActive, recvID)
	}

	// 注册 user-balance:CNY (legacy)
	ubLA := mkLogicalAccount(9001, "user-balance:CNY", false, "")
	lr.byKey["user-balance:CNY"] = ubLA
	lr.byID[9001] = ubLA

	r := NewRouter(lr, ar, rr, accR, nil, nil, func() time.Time { return now })
	return r, lr, ar, rr, accR
}

// 计数器：原子分配唯一 flow_id
var moneyFlowCounter int64

// ============================================================================
// 资金流 1: 用户充值 (Topup)
// 凭证：
//   借: 用户余额 (legacy LA)  — Router 返回 IsLegacy
//   贷: 渠道应收 (rotating)   — Router 写 routing + anchor
//
// 每笔 topup 触发 2 次 Resolve 调用
// ============================================================================
func BenchmarkMoneyFlow_Topup_Serial(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		flowID := "TOPUP_" + strconv.FormatInt(int64(i), 10)
		ch := (i % 100) + 1

		// Entry 1: 用户余额（legacy）
		req1 := &ResolveRequest{
			LogicalAccountKey: "user-balance:CNY",
			FlowID:            flowID,
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeNormal,
		}
		_, err := r.Resolve(ctx, req1)
		if err != nil {
			b.Fatal(err)
		}

		// Entry 2: 渠道应收（rotating）
		req2 := &ResolveRequest{
			LogicalAccountKey: fmt.Sprintf("channel-receivable:ch%d:CNY", ch),
			FlowID:            flowID,
			Direction:         BookingDirectionCredit,
			BookingType:       BookingTypeNormal,
		}
		_, err = r.Resolve(ctx, req2)
		if err != nil {
			b.Fatal(err)
		}
	}
	// 报告：每笔交易（2 个 Resolve）
	b.ReportMetric(float64(2), "resolves/booking")
}

func BenchmarkMoneyFlow_Topup_Parallel(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	atomic.StoreInt64(&moneyFlowCounter, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&moneyFlowCounter, 1)
			flowID := "TOPUP_PAR_" + strconv.FormatInt(n, 10)
			ch := (int(n) % 100) + 1

			req1 := &ResolveRequest{
				LogicalAccountKey: "user-balance:CNY",
				FlowID:            flowID,
				Direction:         BookingDirectionDebit,
				BookingType:       BookingTypeNormal,
			}
			if _, err := r.Resolve(ctx, req1); err != nil {
				b.Fatal(err)
			}

			req2 := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-receivable:ch%d:CNY", ch),
				FlowID:            flowID,
				Direction:         BookingDirectionCredit,
				BookingType:       BookingTypeNormal,
			}
			if _, err := r.Resolve(ctx, req2); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ============================================================================
// 资金流 2: 用户余额支付 (Payment) — 完整 TCC 链路
// Try 阶段建 anchor，Confirm 阶段 update
//
// 每笔支付触发 4 次 Resolve（Try×2 entry + Confirm×2 entry）
// ============================================================================
func BenchmarkMoneyFlow_Payment_TCC_Serial(b *testing.B) {
	r, _, ar, rr, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		flowID := "PAY_" + strconv.FormatInt(int64(i), 10)
		ch := (i % 100) + 1
		laKey := fmt.Sprintf("channel-payable:ch%d:CNY", ch)
		activeNo := fmt.Sprintf("P_CH%d_001", ch)

		// Try 阶段
		// Entry 1: 用户余额 -100 (legacy)
		tryE1 := &ResolveRequest{
			LogicalAccountKey: "user-balance:CNY",
			FlowID:            flowID,
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeTCCTry,
		}
		if _, err := r.Resolve(ctx, tryE1); err != nil {
			b.Fatal(err)
		}
		// Entry 2: 渠道应付 +100 (rotating, 首次锚定)
		tryE2 := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         BookingDirectionCredit,
			BookingType:       BookingTypeTCCTry,
		}
		res2, err := r.Resolve(ctx, tryE2)
		if err != nil {
			b.Fatal(err)
		}
		// 模拟 caller 落 routing + anchor (使 Confirm 走 UpdatePosting 路径)
		laID := int64(1000 + ch)
		rr.byKey[routeKey(flowID, laID)] = res2.RoutePlan.NewRoute
		rr.byKey[routeKey(flowID, laID)].ID = int64(i*10 + 1)
		ar.byKey[anchorKeyB(flowID, activeNo)] = res2.AnchorPlan.NewAnchor
		ar.byKey[anchorKeyB(flowID, activeNo)].ID = int64(i*10 + 2)

		// Confirm 阶段（同 flow_id，应走 UpdatePosting）
		// Entry 1 重复 — 也是 legacy
		_, _ = r.Resolve(ctx, tryE1)
		// Entry 2 — UpdatePosting on existing anchor
		confirmE2 := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         BookingDirectionCredit,
			BookingType:       BookingTypeTCCConfirm,
		}
		if _, err := r.Resolve(ctx, confirmE2); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(4), "resolves/booking")
}

func BenchmarkMoneyFlow_Payment_Try_Parallel(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	atomic.StoreInt64(&moneyFlowCounter, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&moneyFlowCounter, 1)
			flowID := "PAY_PAR_" + strconv.FormatInt(n, 10)
			ch := (int(n) % 100) + 1

			req1 := &ResolveRequest{
				LogicalAccountKey: "user-balance:CNY",
				FlowID:            flowID,
				Direction:         BookingDirectionDebit,
				BookingType:       BookingTypeTCCTry,
			}
			if _, err := r.Resolve(ctx, req1); err != nil {
				b.Fatal(err)
			}
			req2 := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-payable:ch%d:CNY", ch),
				FlowID:            flowID,
				Direction:         BookingDirectionCredit,
				BookingType:       BookingTypeTCCTry,
			}
			if _, err := r.Resolve(ctx, req2); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ============================================================================
// 资金流 3: 用户转账 (Transfer)
// 双 legacy LA，无 routing/anchor 开销
// 每笔转账触发 2 次 Resolve（都是 legacy 短路）
// ============================================================================
func BenchmarkMoneyFlow_Transfer_Serial(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		flowID := "XFER_" + strconv.FormatInt(int64(i), 10)
		req := &ResolveRequest{
			LogicalAccountKey: "user-balance:CNY",
			FlowID:            flowID,
			BookingType:       BookingTypeNormal,
		}
		// 发送方扣款
		req.Direction = BookingDirectionDebit
		if _, err := r.Resolve(ctx, req); err != nil {
			b.Fatal(err)
		}
		// 接收方加款
		req.Direction = BookingDirectionCredit
		if _, err := r.Resolve(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(2), "resolves/booking")
}

func BenchmarkMoneyFlow_Transfer_Parallel(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	atomic.StoreInt64(&moneyFlowCounter, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&moneyFlowCounter, 1)
			flowID := "XFER_PAR_" + strconv.FormatInt(n, 10)
			req := &ResolveRequest{
				LogicalAccountKey: "user-balance:CNY",
				FlowID:            flowID,
				BookingType:       BookingTypeNormal,
			}
			req.Direction = BookingDirectionDebit
			_, _ = r.Resolve(ctx, req)
			req.Direction = BookingDirectionCredit
			_, _ = r.Resolve(ctx, req)
		}
	})
}

// ============================================================================
// 资金流 4: 用户提现 (Withdrawal)
// 用户余额(legacy) → 渠道应付(rotating)
// 与 Topup 镜像（方向相反）
// ============================================================================
func BenchmarkMoneyFlow_Withdrawal_Serial(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		flowID := "WD_" + strconv.FormatInt(int64(i), 10)
		ch := (i % 100) + 1

		req1 := &ResolveRequest{
			LogicalAccountKey: "user-balance:CNY",
			FlowID:            flowID,
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeNormal,
		}
		if _, err := r.Resolve(ctx, req1); err != nil {
			b.Fatal(err)
		}
		req2 := &ResolveRequest{
			LogicalAccountKey: fmt.Sprintf("channel-payable:ch%d:CNY", ch),
			FlowID:            flowID,
			Direction:         BookingDirectionCredit,
			BookingType:       BookingTypeNormal,
		}
		if _, err := r.Resolve(ctx, req2); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(2), "resolves/booking")
}

func BenchmarkMoneyFlow_Withdrawal_Parallel(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	atomic.StoreInt64(&moneyFlowCounter, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&moneyFlowCounter, 1)
			flowID := "WD_PAR_" + strconv.FormatInt(n, 10)
			ch := (int(n) % 100) + 1
			req1 := &ResolveRequest{
				LogicalAccountKey: "user-balance:CNY",
				FlowID:            flowID,
				Direction:         BookingDirectionDebit,
				BookingType:       BookingTypeNormal,
			}
			_, _ = r.Resolve(ctx, req1)
			req2 := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-payable:ch%d:CNY", ch),
				FlowID:            flowID,
				Direction:         BookingDirectionCredit,
				BookingType:       BookingTypeNormal,
			}
			_, _ = r.Resolve(ctx, req2)
		}
	})
}

// ============================================================================
// 混合工作流（生产真实流量模式）
// 业务比例（假设的典型支付公司）：
//   - 充值: 30%
//   - 余额支付: 40%
//   - 转账: 15%
//   - 提现: 15%
// ============================================================================
func BenchmarkMoneyFlow_MixedWorkload_Parallel(b *testing.B) {
	r, _, _, _, _ := buildMoneyFlowFixture()
	ctx := context.Background()
	atomic.StoreInt64(&moneyFlowCounter, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&moneyFlowCounter, 1)
			ch := (int(n) % 100) + 1
			pct := n % 100

			switch {
			case pct < 30: // 30% topup
				flowID := "MIX_TOPUP_" + strconv.FormatInt(n, 10)
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: "user-balance:CNY",
					FlowID:            flowID,
					Direction:         BookingDirectionDebit,
					BookingType:       BookingTypeNormal,
				})
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: fmt.Sprintf("channel-receivable:ch%d:CNY", ch),
					FlowID:            flowID,
					Direction:         BookingDirectionCredit,
					BookingType:       BookingTypeNormal,
				})
			case pct < 70: // 40% payment (Try only, in this bench)
				flowID := "MIX_PAY_" + strconv.FormatInt(n, 10)
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: "user-balance:CNY",
					FlowID:            flowID,
					Direction:         BookingDirectionDebit,
					BookingType:       BookingTypeTCCTry,
				})
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: fmt.Sprintf("channel-payable:ch%d:CNY", ch),
					FlowID:            flowID,
					Direction:         BookingDirectionCredit,
					BookingType:       BookingTypeTCCTry,
				})
			case pct < 85: // 15% transfer
				flowID := "MIX_XFER_" + strconv.FormatInt(n, 10)
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: "user-balance:CNY",
					FlowID:            flowID,
					Direction:         BookingDirectionDebit,
					BookingType:       BookingTypeNormal,
				})
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: "user-balance:CNY",
					FlowID:            flowID,
					Direction:         BookingDirectionCredit,
					BookingType:       BookingTypeNormal,
				})
			default: // 15% withdrawal
				flowID := "MIX_WD_" + strconv.FormatInt(n, 10)
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: "user-balance:CNY",
					FlowID:            flowID,
					Direction:         BookingDirectionDebit,
					BookingType:       BookingTypeNormal,
				})
				_, _ = r.Resolve(ctx, &ResolveRequest{
					LogicalAccountKey: fmt.Sprintf("channel-payable:ch%d:CNY", ch),
					FlowID:            flowID,
					Direction:         BookingDirectionCredit,
					BookingType:       BookingTypeNormal,
				})
			}
		}
	})
}

// ============================================================================
// 既有 flow 高频更新（TCC Confirm/Cancel + 二次记账）
// 真实场景：大量 TCC 完成阶段或多次后续清算
// 这是 UpdatePosting 热路径
// ============================================================================
func BenchmarkMoneyFlow_ExistingFlowHotPath_Parallel(b *testing.B) {
	r, _, ar, rr, _ := buildMoneyFlowFixture()
	ctx := context.Background()

	// 预填 10000 个 active flow（routing + anchor 都已就位）
	const N = 10000
	for i := 0; i < N; i++ {
		ch := (i % 100) + 1
		laID := int64(1000 + ch)
		flowID := "EXISTING_" + strconv.Itoa(i)
		activeNo := fmt.Sprintf("P_CH%d_001", ch)
		rr.byKey[routeKey(flowID, laID)] = &model.FlowAnchorRoute{
			ID: int64(i + 1), FlowID: flowID, LogicalAccountID: laID, AccountNo: activeNo,
		}
		ar.byKey[anchorKeyB(flowID, activeNo)] = &model.TxAccountAnchor{
			ID: int64(i + 100000), FlowID: flowID, LogicalAccountID: laID,
			AccountNo: activeNo, Status: model.AnchorStatusActive, Version: 1,
		}
	}

	atomic.StoreInt64(&moneyFlowCounter, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&moneyFlowCounter, 1)
			idx := int(n) % N
			ch := (idx % 100) + 1
			flowID := "EXISTING_" + strconv.Itoa(idx)

			req := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-payable:ch%d:CNY", ch),
				FlowID:            flowID,
				Direction:         BookingDirectionCredit,
				BookingType:       BookingTypeTCCConfirm,
			}
			if _, err := r.Resolve(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

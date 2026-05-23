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
// Money Flow 压测 — 测量 Router 在不同负载模式下的纯逻辑吞吐
//
// 重要声明：这些 benchmark 使用 fake repo（内存 map），测得是**Router 路由逻辑的
// 上限**，不包括 DB IO。真实生产 TPS 受限于：
//   - MySQL 写入 IOPS（每笔 booking 写 2-3 行：routing + anchor + transaction）
//   - 网络往返（应用 ↔ MySQL）
//   - 锁竞争（uk_flow_account / uk_flow_logical）
//
// 实测建议：
//   1. 用本 bench 数据作为 Router 逻辑天花板
//   2. 真实 DB 压测在 integration 环境跑 1000 TPS × 1h 验证
//   3. 关注 p99 延迟（不只是 throughput）
//
// 运行：
//   go test -bench=BenchmarkRouter -benchmem -benchtime=5s ./internal/service/
//
// 示例输出格式（参考）：
//   BenchmarkRouter_NewFlow_Serial-8         500000   3500 ns/op  ~285k ops/sec
//   BenchmarkRouter_ExistingFlow_Serial-8   1000000   1200 ns/op  ~830k ops/sec
//   BenchmarkRouter_NewFlow_Parallel-8      2000000    800 ns/op  ~1.2M ops/sec
// ============================================================================

// 构造一个干净的 Router fixture，预填 200 个 LA + 对应 active instance。
// 模拟生产规模：200 个渠道（如 alipay/wechat/stripe/...）× 多币种。
func buildBenchFixture() (Router, *fakeLogicalReader, *fakeAnchorReaderB, *fakeRouteReader, *fakeAccountReader) {
	lr := &fakeLogicalReader{
		byKey: make(map[string]*model.LogicalAccount, 200),
		byID:  make(map[int64]*model.LogicalAccount, 200),
	}
	ar := &fakeAnchorReaderB{byKey: make(map[string]*model.TxAccountAnchor, 100000)}
	rr := &fakeRouteReader{byKey: make(map[string]*model.FlowAnchorRoute, 100000)}
	accR := &fakeAccountReader{byNo: make(map[string]*model.Account, 1000)}

	now := time.Now()
	for i := int64(1); i <= 200; i++ {
		key := fmt.Sprintf("channel-payable:bench%d:USD", i)
		activeNo := fmt.Sprintf("ACC_LA%d_ACTIVE", i)
		la := mkLogicalAccount(i, key, true, activeNo)
		lr.byKey[key] = la
		lr.byID[i] = la
		accR.byNo[activeNo] = mkAccountActive(activeNo, i)
	}
	r := NewRouter(lr, ar, rr, accR, nil, nil, func() time.Time { return now })
	return r, lr, ar, rr, accR
}

// ============================================================================
// Bench 1: 新 flow Resolve（首次锚定，无既有 routing/anchor）
// 这是首次记账热路径
// ============================================================================
func BenchmarkRouter_NewFlow_Serial(b *testing.B) {
	r, _, _, _, _ := buildBenchFixture()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		laIdx := (i % 200) + 1
		req := &ResolveRequest{
			LogicalAccountKey: fmt.Sprintf("channel-payable:bench%d:USD", laIdx),
			FlowID:            "FLOW_" + strconv.Itoa(i),
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeNormal,
		}
		_, err := r.Resolve(ctx, req)
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

// ============================================================================
// Bench 2: 既有 flow Resolve（routing + anchor 命中，UpdatePosting 路径）
// 这是 TCC Confirm/Cancel 等后续操作的热路径
// ============================================================================
func BenchmarkRouter_ExistingFlow_Serial(b *testing.B) {
	r, _, ar, rr, _ := buildBenchFixture()
	ctx := context.Background()

	// 预填 10000 个 flow 的 routing + anchor
	const N = 10000
	for i := 0; i < N; i++ {
		laIdx := int64((i % 200) + 1)
		activeNo := fmt.Sprintf("ACC_LA%d_ACTIVE", laIdx)
		flowID := "FLOW_" + strconv.Itoa(i)

		rr.byKey[routeKey(flowID, laIdx)] = &model.FlowAnchorRoute{
			ID: int64(i + 1), FlowID: flowID, LogicalAccountID: laIdx, AccountNo: activeNo,
		}
		ar.byKey[anchorKeyB(flowID, activeNo)] = &model.TxAccountAnchor{
			ID: int64(i + 100000), FlowID: flowID, LogicalAccountID: laIdx,
			AccountNo: activeNo, Status: model.AnchorStatusActive,
			DirectionMask: model.AnchorDirectionDebit, Version: 1,
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		laIdx := (i % 200) + 1
		flowID := "FLOW_" + strconv.Itoa(i%N)
		req := &ResolveRequest{
			LogicalAccountKey: fmt.Sprintf("channel-payable:bench%d:USD", laIdx),
			FlowID:            flowID,
			Direction:         BookingDirectionCredit,
			BookingType:       BookingTypeNormal,
		}
		_, err := r.Resolve(ctx, req)
		if err != nil {
			b.Fatalf("iter %d: %v", i, err)
		}
	}
}

// ============================================================================
// Bench 3: 并行新 flow（高并发首次锚定）
// 模拟峰值流量：高 QPS 下新 flow 涌入
// ============================================================================
func BenchmarkRouter_NewFlow_Parallel(b *testing.B) {
	r, _, _, _, _ := buildBenchFixture()
	ctx := context.Background()

	var counter int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&counter, 1)
			laIdx := (n % 200) + 1
			req := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-payable:bench%d:USD", laIdx),
				FlowID:            "FLOW_PAR_" + strconv.FormatInt(n, 10),
				Direction:         BookingDirectionDebit,
				BookingType:       BookingTypeNormal,
			}
			if _, err := r.Resolve(ctx, req); err != nil {
				b.Fatalf("parallel iter %d: %v", n, err)
			}
		}
	})
}

// ============================================================================
// Bench 4: 并行既有 flow（高并发后续操作）
// 模拟 TCC Confirm/Cancel 高并发
// ============================================================================
func BenchmarkRouter_ExistingFlow_Parallel(b *testing.B) {
	r, _, ar, rr, _ := buildBenchFixture()
	ctx := context.Background()

	const N = 10000
	for i := 0; i < N; i++ {
		laIdx := int64((i % 200) + 1)
		activeNo := fmt.Sprintf("ACC_LA%d_ACTIVE", laIdx)
		flowID := "FLOW_" + strconv.Itoa(i)
		rr.byKey[routeKey(flowID, laIdx)] = &model.FlowAnchorRoute{
			ID: int64(i + 1), FlowID: flowID, LogicalAccountID: laIdx, AccountNo: activeNo,
		}
		ar.byKey[anchorKeyB(flowID, activeNo)] = &model.TxAccountAnchor{
			ID: int64(i + 100000), FlowID: flowID, LogicalAccountID: laIdx,
			AccountNo: activeNo, Status: model.AnchorStatusActive,
			DirectionMask: model.AnchorDirectionDebit, Version: 1,
		}
	}

	var counter int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&counter, 1)
			laIdx := (n % 200) + 1
			flowID := "FLOW_" + strconv.FormatInt(n%N, 10)
			req := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-payable:bench%d:USD", laIdx),
				FlowID:            flowID,
				Direction:         BookingDirectionCredit,
				BookingType:       BookingTypeNormal,
			}
			if _, err := r.Resolve(ctx, req); err != nil {
				b.Fatalf("parallel iter %d: %v", n, err)
			}
		}
	})
}

// ============================================================================
// Bench 5: 实际生产模式（mixed workload）
//   80% existing flow + 20% new flow
// 这是大部分支付流的实际比例：TCC Try（新 flow）+ Confirm/Cancel（既有 flow） + 业务流（既有）
// ============================================================================
func BenchmarkRouter_MixedWorkload_Parallel(b *testing.B) {
	r, _, ar, rr, _ := buildBenchFixture()
	ctx := context.Background()

	// 预填 5000 个 existing flow
	const N = 5000
	for i := 0; i < N; i++ {
		laIdx := int64((i % 200) + 1)
		activeNo := fmt.Sprintf("ACC_LA%d_ACTIVE", laIdx)
		flowID := "EXIST_" + strconv.Itoa(i)
		rr.byKey[routeKey(flowID, laIdx)] = &model.FlowAnchorRoute{
			ID: int64(i + 1), FlowID: flowID, LogicalAccountID: laIdx, AccountNo: activeNo,
		}
		ar.byKey[anchorKeyB(flowID, activeNo)] = &model.TxAccountAnchor{
			ID: int64(i + 100000), FlowID: flowID, LogicalAccountID: laIdx,
			AccountNo: activeNo, Status: model.AnchorStatusActive, Version: 1,
		}
	}

	var counter int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&counter, 1)
			laIdx := (n % 200) + 1
			req := &ResolveRequest{
				LogicalAccountKey: fmt.Sprintf("channel-payable:bench%d:USD", laIdx),
				Direction:         BookingDirectionDebit,
				BookingType:       BookingTypeNormal,
			}
			if n%5 == 0 {
				// 20% 新 flow
				req.FlowID = "NEW_" + strconv.FormatInt(n, 10)
			} else {
				// 80% 既有 flow
				req.FlowID = "EXIST_" + strconv.FormatInt(n%N, 10)
			}
			if _, err := r.Resolve(ctx, req); err != nil {
				b.Fatalf("iter %d: %v", n, err)
			}
		}
	})
}

// ============================================================================
// Bench 6: TCC 完整链路（Try → Confirm）
// 模拟一个 flow 的完整生命周期
// ============================================================================
func BenchmarkRouter_TCCLifecycle_Serial(b *testing.B) {
	r, _, ar, rr, accR := buildBenchFixture()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		laIdx := int64((i % 200) + 1)
		laKey := fmt.Sprintf("channel-payable:bench%d:USD", laIdx)
		flowID := "TCC_FLOW_" + strconv.Itoa(i)

		// Try: 首次锚定
		tryReq := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         BookingDirectionCredit,
			BookingType:       BookingTypeTCCTry,
		}
		res1, err := r.Resolve(ctx, tryReq)
		if err != nil {
			b.Fatal(err)
		}

		// 模拟 caller 写入 routing + anchor
		rr.byKey[routeKey(flowID, laIdx)] = res1.RoutePlan.NewRoute
		rr.byKey[routeKey(flowID, laIdx)].ID = int64(i + 1000000)
		ar.byKey[anchorKeyB(flowID, res1.AccountNo)] = res1.AnchorPlan.NewAnchor
		ar.byKey[anchorKeyB(flowID, res1.AccountNo)].ID = int64(i + 2000000)
		ar.byKey[anchorKeyB(flowID, res1.AccountNo)].Version = 0

		// Confirm: 同 flow 二次记账
		confirmReq := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            flowID,
			Direction:         BookingDirectionDebit,
			BookingType:       BookingTypeTCCConfirm,
		}
		_, err = r.Resolve(ctx, confirmReq)
		if err != nil {
			b.Fatal(err)
		}
	}
	_ = accR
}

// ============================================================================
// Bench 7: Refund 退款继承
// ============================================================================
func BenchmarkRouter_Refund_Serial(b *testing.B) {
	r, _, _, rr, _ := buildBenchFixture()
	ctx := context.Background()

	// 预填 1000 个源 flow 的 routing
	const N = 1000
	for i := 0; i < N; i++ {
		laIdx := int64((i % 200) + 1)
		activeNo := fmt.Sprintf("ACC_LA%d_ACTIVE", laIdx)
		flowID := "PAY_" + strconv.Itoa(i)
		rr.byKey[routeKey(flowID, laIdx)] = &model.FlowAnchorRoute{
			ID: int64(i + 1), FlowID: flowID, LogicalAccountID: laIdx, AccountNo: activeNo,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		laIdx := (i % 200) + 1
		laKey := fmt.Sprintf("channel-payable:bench%d:USD", laIdx)
		req := &ResolveRequest{
			LogicalAccountKey: laKey,
			FlowID:            "REFUND_" + strconv.Itoa(i),
			Direction:         BookingDirectionDebit,
			OriginalFlowID:    "PAY_" + strconv.Itoa(i%N),
			ReuseSource:       model.AnchorReuseSourceRefundOf,
			BookingType:       BookingTypeNormal,
		}
		if _, err := r.Resolve(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

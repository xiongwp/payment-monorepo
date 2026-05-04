package service

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/repo"
)

// fake* mocks 把对应 Repository 接口 embedded，仅 override 测试涉及的方法；
// 其它方法走 nil interface 任意调用都 panic 暴露误用（如 webhook idempotency
// test 同模式）。

// fakeRefundRepo 用一个原子计数器追踪 CASUpdateStatus 每次的输入和 won 结果。
// 第一次调 CAS 返 (rf, true)，之后所有调用返 (rf, false) ——模拟 status 已经
// 被并发 caller 推到 succeeded 之后的语义。
type fakeRefundRepo struct {
	repo.RefundRepository
	cur            *domain.Refund
	casCalls       atomic.Int32
	casFromHistory []domain.RefundStatus // 记每次 CAS 的 from 参数
}

func (f *fakeRefundRepo) CASUpdateStatus(_ context.Context, _, _ string,
	from, to domain.RefundStatus, _ map[string]any) (*domain.Refund, bool, error) {
	n := f.casCalls.Add(1)
	f.casFromHistory = append(f.casFromHistory, from)
	if n == 1 {
		// 第一次：行的 status 真的是 from → CAS 命中 → 推到 to
		f.cur.Status = to
		return f.cur, true, nil
	}
	// 第二次起：行的 status 已经是 to（被前一次推过去了）→ WHERE status=from
	// 不命中 → won=false。返回当前 row 的快照（status==to）。
	return f.cur, false, nil
}

func (f *fakeRefundRepo) Get(_ context.Context, _, _ string) (*domain.Refund, error) {
	return f.cur, nil
}

// fakeChargeRepo 累加 amount_refunded；单测最关键的不变量就是这个数字
// 调 N 次 MarkSucceeded 后只长了 1×rf.Amount。
type fakeChargeRepo struct {
	repo.ChargeRepository
	cur                *domain.Charge
	amountRefundedCalls atomic.Int32
}

func (f *fakeChargeRepo) UpdateFields(_ context.Context, _, _ string, fields map[string]any) (*domain.Charge, error) {
	// SQL 表达式 'amount_refunded + ?' 在测试里没法真的 eval —— 我们用 calls 计数
	// 替代验证：每次调用就 +1，业务断言是 calls==1（即只在 CAS 赢时进副作用）。
	if _, ok := fields["amount_refunded"]; ok {
		f.amountRefundedCalls.Add(1)
	}
	return f.cur, nil
}

func (f *fakeChargeRepo) Get(_ context.Context, _, _ string) (*domain.Charge, error) {
	return f.cur, nil
}

type fakePIRepo struct {
	repo.PaymentIntentRepository
	cur            *domain.PaymentIntent
	updateFieldsN  atomic.Int32
}

func (f *fakePIRepo) Get(_ context.Context, _ string) (*domain.PaymentIntent, error) {
	return f.cur, nil
}
func (f *fakePIRepo) UpdateFields(_ context.Context, _ string, _ map[string]any) (*domain.PaymentIntent, error) {
	f.updateFieldsN.Add(1)
	return f.cur, nil
}

// fakeAccounting 计 EnqueueRefundSucceeded 调用次数。
type fakeAccounting struct {
	enqueueRefundCalls atomic.Int32
}

func (f *fakeAccounting) EnqueueChargeSucceeded(_ context.Context, _ *domain.PaymentIntent, _ string, _ int64) error {
	return nil
}
func (f *fakeAccounting) EnqueueRefundSucceeded(_ context.Context, _ *domain.PaymentIntent, _ *domain.Refund) error {
	f.enqueueRefundCalls.Add(1)
	return nil
}

// 兼容 db helper（chargeRepo.UpdateFields 内部用 gorm.Expr，Mock 路径不需要）
var _ = gorm.DB{}

// 资损回归：MarkSucceeded 对同 (piID, refundID) 多次调用，副作用只跑一次。
//
// 旧行为（PR #26 之前）：refundService.MarkSucceeded 用非 CAS UpdateFields →
// 并发 webhook + RefundRetryWorker 各调一次 → amount_refunded 跑两次累加 →
// charge 余额翻倍 → 后续 SumActiveByCharge 误判超额 → 商户无法继续退款。
//
// 新行为（PR #26 之后）：CAS pending → succeeded，第二次 won=false → skip
// 副作用块（amount_refunded 累加 / refund_phase / accounting outbox）。
func TestRefundService_MarkSucceeded_DoubleCallOnlyRunsSideEffectsOnce(t *testing.T) {
	rf := &domain.Refund{
		ID:              "re_test_1",
		PaymentIntentID: "pi_test_1",
		ChargeID:        "ch_test_1",
		Amount:          10000,
		Status:          domain.RefundStatusPending,
	}
	ch := &domain.Charge{
		ID:              "ch_test_1",
		PaymentIntentID: "pi_test_1",
		Amount:          50000,
		AmountCaptured:  50000,
	}
	pi := &domain.PaymentIntent{
		ID:     "pi_test_1",
		Status: domain.PIStatusSucceeded,
	}

	rfRepo := &fakeRefundRepo{cur: rf}
	chRepo := &fakeChargeRepo{cur: ch}
	piRepo := &fakePIRepo{cur: pi}
	acc := &fakeAccounting{}

	svc := &refundService{
		piRepo:     piRepo,
		chargeRepo: chRepo,
		refundRepo: rfRepo,
		accounting: acc,
		logger:     zap.NewNop(),
	}

	// 第一次：CAS 命中 → 副作用应该跑
	if _, err := svc.MarkSucceeded(context.Background(), "pi_test_1", "re_test_1"); err != nil {
		t.Fatalf("first MarkSucceeded: %v", err)
	}
	// 第二次（模拟 webhook + RefundRetryWorker race）：CAS won=false → 副作用跳过
	if _, err := svc.MarkSucceeded(context.Background(), "pi_test_1", "re_test_1"); err != nil {
		t.Fatalf("second MarkSucceeded: %v", err)
	}

	if got := chRepo.amountRefundedCalls.Load(); got != 1 {
		t.Fatalf("amount_refunded should be incremented exactly once, got %d "+
			"(regression: 双计 → charge.amount_refunded 翻倍 → 商户无法继续退款)", got)
	}
	if got := acc.enqueueRefundCalls.Load(); got != 1 {
		t.Fatalf("EnqueueRefundSucceeded should be called once, got %d "+
			"(BFF 端虽然有 RequestID UNIQUE 兜底，但应该上层就拦掉)", got)
	}
	// 两次 CAS 都跑了（refundService 没在 won=false 时短路 CAS 调用本身），fromHistory
	// 应该是 [Pending, Pending] —— 两次都用 Pending 当 from（因为 fakeRefundRepo
	// 不 mutate cur.Status until won case）。这是 design：服务层不缓存"我已经知道
	// 是终态了"，依赖 repo CAS 兜底。
	if rfRepo.casCalls.Load() != 2 {
		t.Fatalf("CAS should be called twice (caller-side blind, repo bottoms out), got %d", rfRepo.casCalls.Load())
	}
}

// MarkFailed 同模式：双调时 RefundPhase=REFUND_FAILED 副作用只跑一次。
func TestRefundService_MarkFailed_DoubleCallOnlyRunsSideEffectsOnce(t *testing.T) {
	rf := &domain.Refund{
		ID:              "re_f_1",
		PaymentIntentID: "pi_f_1",
		ChargeID:        "ch_f_1",
		Amount:          5000,
		Status:          domain.RefundStatusPending,
	}
	pi := &domain.PaymentIntent{ID: "pi_f_1"}
	rfRepo := &fakeRefundRepo{cur: rf}
	piRepo := &fakePIRepo{cur: pi}

	svc := &refundService{
		piRepo:     piRepo,
		refundRepo: rfRepo,
		logger:     zap.NewNop(),
	}

	if _, err := svc.MarkFailed(context.Background(), "pi_f_1", "re_f_1", "test"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := svc.MarkFailed(context.Background(), "pi_f_1", "re_f_1", "test"); err != nil {
		t.Fatalf("second: %v", err)
	}
	// piRepo.UpdateFields(refund_phase=REFUND_FAILED) 只能跑 1 次。
	// 两次跑会让 refund_phase 多更新一次（idempotent on 同值，但跨 caller 可能会
	// 把 succeeded 的 phase 错误覆盖）。
	if got := piRepo.updateFieldsN.Load(); got != 1 {
		t.Fatalf("PI.UpdateFields(refund_phase) should be called once, got %d", got)
	}
}

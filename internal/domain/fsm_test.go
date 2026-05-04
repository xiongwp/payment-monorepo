package domain

import "testing"

func TestPICanTransition(t *testing.T) {
	ok := func(from, to PaymentIntentStatus) {
		t.Helper()
		if !CanTransition(from, to) {
			t.Errorf("expected %s → %s allowed", from, to)
		}
	}
	fail := func(from, to PaymentIntentStatus) {
		t.Helper()
		if CanTransition(from, to) {
			t.Errorf("expected %s → %s blocked", from, to)
		}
	}

	ok(PIStatusCreated, PIStatusRequiresAction)
	ok(PIStatusCreated, PIStatusProcessing)
	ok(PIStatusCreated, PIStatusCanceled)
	ok(PIStatusRequiresAction, PIStatusProcessing)
	ok(PIStatusRequiresAction, PIStatusRequiresAction) // 自环
	ok(PIStatusRequiresAction, PIStatusCanceled)
	ok(PIStatusProcessing, PIStatusSucceeded)
	ok(PIStatusProcessing, PIStatusFailed)

	// 终态不可再转
	fail(PIStatusSucceeded, PIStatusFailed)
	fail(PIStatusFailed, PIStatusProcessing)
	fail(PIStatusCanceled, PIStatusProcessing)

	// 跨越阶段的非法跳转
	fail(PIStatusCreated, PIStatusSucceeded)
	fail(PIStatusProcessing, PIStatusCreated)
	fail(PIStatusRequiresAction, PIStatusSucceeded)
}

func TestRefundPhaseTransition(t *testing.T) {
	ok := func(from, to RefundPhase) {
		t.Helper()
		if !CanRefundPhaseTransition(from, to) {
			t.Errorf("expected %s → %s allowed", from, to)
		}
	}
	fail := func(from, to RefundPhase) {
		t.Helper()
		if CanRefundPhaseTransition(from, to) {
			t.Errorf("expected %s → %s blocked", from, to)
		}
	}

	ok(RefundPhaseNone, RefundPhaseRefunding)
	ok(RefundPhaseRefunding, RefundPhaseSucceededAlias())
	ok(RefundPhaseRefunding, RefundPhasePartiallyRefunded)
	ok(RefundPhaseRefunding, RefundPhaseRefundFailed)
	ok(RefundPhasePartiallyRefunded, RefundPhaseRefunding)
	ok(RefundPhaseRefundFailed, RefundPhaseRefunding)

	fail(RefundPhaseFullyRefunded, RefundPhaseRefunding) // 终态
	fail(RefundPhaseNone, RefundPhaseFullyRefunded)      // 不能直接跳
	fail(RefundPhaseRefunding, RefundPhaseNone)          // 不能回退
}

// RefundPhaseSucceededAlias 测试辅助：状态机里"退款全额成功"状态
func RefundPhaseSucceededAlias() RefundPhase { return RefundPhaseFullyRefunded }

func TestRefundActionTransition(t *testing.T) {
	if CanActionTransition(PayActionStatusSucceeded, PayActionStatusPending) {
		t.Errorf("succeeded is terminal")
	}
	if !CanActionTransition(PayActionStatusPending, PayActionStatusSucceeded) {
		t.Errorf("pending→succeeded must be allowed")
	}
	if !CanActionTransition(PayActionStatusPending, PayActionStatusFailed) {
		t.Errorf("pending→failed must be allowed")
	}
	if !CanActionTransition(PayActionStatusPending, PayActionStatusExpired) {
		t.Errorf("pending→expired must be allowed")
	}
}

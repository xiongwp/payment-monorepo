// charge_lifecycle_test.go — payment-core 关键路径 (charge / capture / refund) 状态机.
//
// 状态机:
//   created → authorized → captured → succeeded
//                                ↘ refund_pending → refunded
//          ↘ declined (issuer 拒)
//          ↘ failed (业务侧错)
//
// 这是 PaymentIntent 风格. Stripe-equivalent.

package payment_core_test

import (
	"testing"
)

type ChargeStatus string

const (
	StatusCreated      ChargeStatus = "created"
	StatusAuthorized   ChargeStatus = "authorized"
	StatusCaptured     ChargeStatus = "captured"
	StatusSucceeded    ChargeStatus = "succeeded"
	StatusRefundPending ChargeStatus = "refund_pending"
	StatusRefunded     ChargeStatus = "refunded"
	StatusDeclined     ChargeStatus = "declined"
	StatusFailed       ChargeStatus = "failed"
	StatusVoided       ChargeStatus = "voided"
)

func canTransit(from, to ChargeStatus) bool {
	allowed := map[ChargeStatus][]ChargeStatus{
		StatusCreated:       {StatusAuthorized, StatusDeclined, StatusFailed},
		StatusAuthorized:    {StatusCaptured, StatusVoided, StatusFailed},
		StatusCaptured:      {StatusSucceeded, StatusRefundPending, StatusFailed},
		StatusSucceeded:     {StatusRefundPending},
		StatusRefundPending: {StatusRefunded, StatusFailed},
	}
	for _, t := range allowed[from] {
		if t == to {
			return true
		}
	}
	return false
}

func TestCharge_HappyPath(t *testing.T) {
	for _, p := range [][]ChargeStatus{
		{StatusCreated, StatusAuthorized, StatusCaptured, StatusSucceeded},
		{StatusCreated, StatusAuthorized, StatusCaptured, StatusSucceeded, StatusRefundPending, StatusRefunded},
	} {
		for i := 1; i < len(p); i++ {
			if !canTransit(p[i-1], p[i]) {
				t.Errorf("%s → %s should allow", p[i-1], p[i])
			}
		}
	}
}

func TestCharge_AuthorizedCanVoid(t *testing.T) {
	if !canTransit(StatusAuthorized, StatusVoided) {
		t.Error("authorized → voided (取消授权) should allow")
	}
}

func TestCharge_RejectsBackward(t *testing.T) {
	bad := []struct{ from, to ChargeStatus }{
		{StatusCaptured, StatusAuthorized},
		{StatusSucceeded, StatusCreated},
		{StatusRefunded, StatusSucceeded},
	}
	for _, c := range bad {
		if canTransit(c.from, c.to) {
			t.Errorf("%s → %s should NOT (backwards)", c.from, c.to)
		}
	}
}

func TestCharge_DeclinedTerminal(t *testing.T) {
	if canTransit(StatusDeclined, StatusAuthorized) {
		t.Error("declined is terminal (issuer 拒, 不能重试)")
	}
}

func TestCharge_VoidedTerminal(t *testing.T) {
	if canTransit(StatusVoided, StatusCaptured) {
		t.Error("voided is terminal")
	}
}

func TestCharge_RefundedTerminal(t *testing.T) {
	if canTransit(StatusRefunded, StatusSucceeded) {
		t.Error("refunded is terminal")
	}
}

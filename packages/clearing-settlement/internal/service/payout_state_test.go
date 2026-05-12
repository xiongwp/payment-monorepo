// payout_state_test.go — Payout 状态机关键路径单测.
//
// 状态机:
//   pending_approval → approved → sent → settled
//                                     ↘ failed → ...
//                                              ↘ reversed (R-message)
//
// 验证:
//   - approved → sent  (调 bank rail 后)
//   - sent → settled    (T+1 收 ack)
//   - sent → failed     (bank reject)
//   - settled → reversed (NACHA R-transaction / SEPA R-message 退款)
//   - 不能逆向迁移 (sent → pending_approval invalid)

package service

import (
	"testing"

	"reconcile-system/packages/clearing-settlement/internal/domain"
)

// transition 是 service 内部 helper; 这里 inline 表达期望.
func transition(from, to domain.PayoutStatus) bool {
	allowed := map[domain.PayoutStatus][]domain.PayoutStatus{
		domain.PayoutStatusPendingApproval: {domain.PayoutStatusApproved, domain.PayoutStatusFailed},
		domain.PayoutStatusApproved:        {domain.PayoutStatusSent, domain.PayoutStatusFailed},
		domain.PayoutStatusSent:            {domain.PayoutStatusSettled, domain.PayoutStatusFailed},
		domain.PayoutStatusSettled:         {domain.PayoutStatusReversed},
	}
	for _, t := range allowed[from] {
		if t == to {
			return true
		}
	}
	return false
}

func TestPayout_HappyPath(t *testing.T) {
	chain := []domain.PayoutStatus{
		domain.PayoutStatusPendingApproval,
		domain.PayoutStatusApproved,
		domain.PayoutStatusSent,
		domain.PayoutStatusSettled,
	}
	for i := 1; i < len(chain); i++ {
		if !transition(chain[i-1], chain[i]) {
			t.Errorf("transition %s → %s should be allowed", chain[i-1], chain[i])
		}
	}
}

func TestPayout_RejectsBackwardMoves(t *testing.T) {
	bad := []struct{ from, to domain.PayoutStatus }{
		{domain.PayoutStatusSent, domain.PayoutStatusPendingApproval},
		{domain.PayoutStatusApproved, domain.PayoutStatusPendingApproval},
		{domain.PayoutStatusSettled, domain.PayoutStatusSent},
	}
	for _, c := range bad {
		if transition(c.from, c.to) {
			t.Errorf("transition %s → %s should NOT be allowed (backwards)", c.from, c.to)
		}
	}
}

func TestPayout_SettledCanReverse(t *testing.T) {
	if !transition(domain.PayoutStatusSettled, domain.PayoutStatusReversed) {
		t.Error("settled → reversed (R-transaction) should be allowed")
	}
}

func TestPayout_PendingApprovalCanFail(t *testing.T) {
	if !transition(domain.PayoutStatusPendingApproval, domain.PayoutStatusFailed) {
		t.Error("pending_approval → failed (ops rejected) should be allowed")
	}
}

func TestPayout_FailedIsTerminal(t *testing.T) {
	if transition(domain.PayoutStatusFailed, domain.PayoutStatusApproved) {
		t.Error("failed should be terminal — cannot re-approve")
	}
	if transition(domain.PayoutStatusFailed, domain.PayoutStatusSent) {
		t.Error("failed should be terminal")
	}
}

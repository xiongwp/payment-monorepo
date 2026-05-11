// refund domain state machine 测试。
//
// 状态机正确性是 P0 — 一旦 valid_transition 写错，可能出现"一笔退款多次到账"
// 或"退款已完成又被改 failed"等资金事故。

package domain

import "testing"

func TestValidTransition(t *testing.T) {
	cases := []struct {
		from, to Status
		ok       bool
	}{
		// 合法迁移
		{StatusRequested, StatusApproved, true},
		{StatusRequested, StatusRejected, true},
		{StatusApproved, StatusSubmitted, true},
		{StatusApproved, StatusFailed, true},
		{StatusSubmitted, StatusCompleted, true},
		{StatusSubmitted, StatusFailed, true},

		// 跳过中间态 — 不允许
		{StatusRequested, StatusCompleted, false},
		{StatusRequested, StatusSubmitted, false},
		{StatusApproved, StatusCompleted, false},
		{StatusApproved, StatusRejected, false}, // 已 approved 不能再 reject

		// 终态不可再迁
		{StatusCompleted, StatusFailed, false},
		{StatusCompleted, StatusApproved, false},
		{StatusFailed, StatusCompleted, false},
		{StatusRejected, StatusApproved, false},

		// 反向（防回退）
		{StatusSubmitted, StatusApproved, false},
		{StatusApproved, StatusRequested, false},
	}
	for _, c := range cases {
		got := ValidTransition(c.from, c.to)
		if got != c.ok {
			t.Errorf("Transition(%s → %s): got %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminals := []Status{StatusCompleted, StatusFailed, StatusRejected}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	nonTerminals := []Status{StatusRequested, StatusApproved, StatusSubmitted}
	for _, s := range nonTerminals {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

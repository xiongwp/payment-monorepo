// dispute 状态机测试。

package domain

import "testing"

func TestValidTransition(t *testing.T) {
	cases := []struct {
		from, to DisputeStatus
		ok       bool
	}{
		// 合法
		{StatusNeedsResponse, StatusEvidenceSubmitted, true},
		{StatusNeedsResponse, StatusExpired, true},
		{StatusNeedsResponse, StatusLost, true},
		{StatusNeedsResponse, StatusVoided, true},
		{StatusEvidenceSubmitted, StatusWon, true},
		{StatusEvidenceSubmitted, StatusLost, true},
		{StatusEvidenceSubmitted, StatusArbitration, true},
		{StatusEvidenceSubmitted, StatusVoided, true},
		{StatusArbitration, StatusWon, true},
		{StatusArbitration, StatusLost, true},

		// 非法 — 跳过 evidence 阶段
		{StatusNeedsResponse, StatusWon, false},
		{StatusNeedsResponse, StatusArbitration, false},

		// 终态不可迁
		{StatusWon, StatusLost, false},
		{StatusLost, StatusWon, false},
		{StatusExpired, StatusWon, false},
		{StatusVoided, StatusNeedsResponse, false},
	}
	for _, c := range cases {
		got := ValidTransition(c.from, c.to)
		if got != c.ok {
			t.Errorf("%s → %s: got %v want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	terminals := []DisputeStatus{StatusWon, StatusLost, StatusExpired, StatusVoided}
	for _, s := range terminals {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	live := []DisputeStatus{StatusNeedsResponse, StatusEvidenceSubmitted, StatusArbitration}
	for _, s := range live {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

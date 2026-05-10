package diffstate

import "testing"

func TestIDFor_Stable(t *testing.T) {
	a := IDFor("s1", "r1", "amount_mismatch", "pi_xxx", 0)
	b := IDFor("s1", "r1", "amount_mismatch", "pi_xxx", 0)
	if a != b {
		t.Fatalf("IDFor not stable: %s vs %s", a, b)
	}
	c := IDFor("s1", "r1", "amount_mismatch", "pi_yyy", 0)
	if a == c {
		t.Fatalf("different key produced same ID")
	}
}

func TestState_IsTerminal(t *testing.T) {
	for _, s := range []State{StateResolved, StateFalsePositive, StateExpired} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []State{StateOpen, StateAcked} {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to State
		ok       bool
	}{
		// open → 任何非空目标都允许
		{StateOpen, StateAcked, true},
		{StateOpen, StateResolved, true},
		{StateOpen, StateFalsePositive, true},
		{StateOpen, StateExpired, true},
		// acked → 终态
		{StateAcked, StateResolved, true},
		{StateAcked, StateFalsePositive, true},
		{StateAcked, StateExpired, true},
		// acked → open（回退）不允许
		{StateAcked, StateOpen, false},
		// 终态不可迁
		{StateResolved, StateOpen, false},
		{StateResolved, StateAcked, false},
		{StateFalsePositive, StateOpen, false},
		{StateExpired, StateOpen, false},
	}
	for _, c := range cases {
		got := canTransition(c.from, c.to)
		if got != c.ok {
			t.Errorf("canTransition(%s, %s) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestValidTransition(t *testing.T) {
	for _, s := range []State{StateOpen, StateAcked, StateResolved, StateFalsePositive, StateExpired} {
		if !validTransition(s) {
			t.Errorf("%s should be valid", s)
		}
	}
	if validTransition("totally_made_up") {
		t.Errorf("unknown state should be invalid")
	}
}

package pii

import "testing"

func TestMaskEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a@b.com", "***@b.com"},
		{"ab@b.com", "a*@b.com"},
		{"abcdef@b.com", "a***f@b.com"},
		{"john.doe@example.org", "j***e@example.org"},
		{"no-at-sign", "***"},
		{"trailing@", "***"},
		{"", "***"},
	}
	for _, c := range cases {
		if got := MaskEmail(c.in); got != c.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"+639171234567", "+63******4567"},
		{"09171234567", "091****4567"},
		{"+63 917-123 4567", "+63******4567"}, // 分隔符被剥离
		{"123", "***"},
		{"", "***"},
	}
	for _, c := range cases {
		if got := MaskPhone(c.in); got != c.want {
			t.Errorf("MaskPhone(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskTail(t *testing.T) {
	cases := []struct {
		in   string
		keep int
		want string
	}{
		{"sk_live_abcdef1234", 4, "**************1234"},
		{"AB", 4, "***"},
		{"P12345678", 4, "*****5678"},
		{"", 4, "***"},
	}
	for _, c := range cases {
		if got := MaskTail(c.in, c.keep); got != c.want {
			t.Errorf("MaskTail(%q, %d) = %q, want %q", c.in, c.keep, got, c.want)
		}
	}
}

package validatex

import (
	"errors"
	"strings"
	"testing"
)

func TestAll_AllPass(t *testing.T) {
	if err := All(
		Required("name", "bob"),
		Email("email", "bob@example.com"),
	); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

func TestAll_AccumulatesFailures(t *testing.T) {
	err := All(
		Required("name", ""),
		Email("email", "not-an-email"),
		CountryISO2("country", "usa"),
	)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want errors.Is(ErrValidation), got %T", err)
	}
	msg := err.Error()
	for _, want := range []string{"name", "email", "country"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in msg: %s", want, msg)
		}
	}
}

func TestRequired(t *testing.T) {
	if Required("x", "  ")() == nil {
		t.Fatal("whitespace should fail Required")
	}
	if Required("x", "y")() != nil {
		t.Fatal("non-empty should pass")
	}
}

func TestMaxLen(t *testing.T) {
	if MaxLen("x", "12345", 3)() == nil {
		t.Fatal("over-limit should fail")
	}
	if MaxLen("x", "12", 3)() != nil {
		t.Fatal("under-limit should pass")
	}
}

func TestLen_SkipsEmpty(t *testing.T) {
	if Len("x", "", 3)() != nil {
		t.Fatal("empty should pass Len (Required handles empty)")
	}
	if Len("x", "ab", 3)() == nil {
		t.Fatal("wrong length should fail")
	}
}

func TestEmail(t *testing.T) {
	cases := []struct {
		in   string
		want bool // true = pass
	}{
		{"", true}, // empty passes
		{"a@b.c", true},
		{"no-at-sign", false},
		{"a@no-dot", false},
		{"has space@x.y", false},
	}
	for _, c := range cases {
		got := Email("e", c.in)() == nil
		if got != c.want {
			t.Errorf("Email(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCountryISO2(t *testing.T) {
	if CountryISO2("c", "PH")() != nil {
		t.Error("PH should pass")
	}
	if CountryISO2("c", "ph")() == nil {
		t.Error("lowercase should fail")
	}
	if CountryISO2("c", "PHL")() == nil {
		t.Error("3 chars should fail")
	}
	if CountryISO2("c", "")() != nil {
		t.Error("empty should pass (Required handles empty)")
	}
}

func TestCurrencyISO4217(t *testing.T) {
	if CurrencyISO4217("c", "PHP")() != nil {
		t.Error("PHP should pass")
	}
	if CurrencyISO4217("c", "php")() == nil {
		t.Error("lowercase should fail")
	}
}

func TestHTTPURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"https://example.com/wh", true},
		{"http://x", true},
		{"ftp://x", false},
		{"https://", false},
		{"not a url", false},
	}
	for _, c := range cases {
		got := HTTPURL("u", c.in)() == nil
		if got != c.want {
			t.Errorf("HTTPURL(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOneOf(t *testing.T) {
	rule := OneOf("kind", "live", "live", "test")
	if rule() != nil { // "live" placeholder isn't given because value is empty; empty passes
		t.Error("empty should pass")
	}
	if OneOf("k", "live", "live", "test")() != nil {
		t.Error("live should pass")
	}
	if OneOf("k", "prod", "live", "test")() == nil {
		t.Error("prod should fail")
	}
}

func TestInt64Range(t *testing.T) {
	if Int64Range("n", 5, 1, 10)() != nil {
		t.Error("in range should pass")
	}
	if Int64Range("n", -1, 0, 10)() == nil {
		t.Error("below min should fail")
	}
	if Int64Range("n", 11, 0, 10)() == nil {
		t.Error("above max should fail")
	}
}

func TestPhone(t *testing.T) {
	good := []string{
		"", // empty optional
		"+639171234567",
		"09171234567",
		"+63 917-123 4567",
		"+1 (555) 123-4567",
	}
	for _, p := range good {
		if err := Phone("contact_phone", p)(); err != nil {
			t.Errorf("Phone(%q) should pass, got %v", p, err)
		}
	}
	bad := []string{
		"abc123",                 // 字母
		"+ 1 2",                  // 太短（数字 < 7）
		"123",                    // 太短
		"+123456789012345678901", // 21 digits
		"+86🤖12345678",           // emoji
		"+1*5551234567",          // 星号不在白名单
	}
	for _, p := range bad {
		if Phone("contact_phone", p)() == nil {
			t.Errorf("Phone(%q) should fail", p)
		}
	}
}

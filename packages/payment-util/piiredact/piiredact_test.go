package piiredact

import (
	"strings"
	"testing"
)

func TestMaskValue(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"4111111111111111", "411111******1111"},   // 16-digit card
		{"4111111111111", "411111***1111"},         // 13-digit card
		{"alice@example.com", "a***@example.com"},  // email
		{"+8613912345678", "+8***78"},              // phone (not card-shape)
		{"abc", "***"},
		{"abcde", "a***e"},
		{"abcdefghij", "a***j"},
		{"abcdefghijk", "ab***jk"},
		{"", ""},
	}
	for _, c := range cases {
		got := maskValue(c.in)
		if got != c.want {
			t.Errorf("maskValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactStruct_TagWins(t *testing.T) {
	type User struct {
		Name      string `json:"name"`
		Email     string `json:"email"`            // matched by name rule (mask)
		Password  string `json:"password"`         // matched by name rule (drop)
		SafeField string `json:"safe_field" pii:"skip"`
		Secret    string `json:"foo" pii:"drop"`   // tag overrides
		Public    string `json:"public"`
	}
	r := Default()
	out := r.Redact(User{
		Name:      "Alice",
		Email:     "alice@example.com",
		Password:  "supersecret",
		SafeField: "safe",
		Secret:    "should drop",
		Public:    "public info",
	}).(map[string]any)

	if out["password"] != "<redacted>" {
		t.Errorf("password should be redacted, got %v", out["password"])
	}
	if out["email"] != "a***@example.com" {
		t.Errorf("email should be masked, got %v", out["email"])
	}
	if out["foo"] != "<redacted>" {
		t.Errorf("Secret tag-drop should produce <redacted>, got %v", out["foo"])
	}
	if out["safe_field"] != "safe" {
		t.Errorf("safe_field with pii:skip should passthrough, got %v", out["safe_field"])
	}
	if out["public"] != "public info" {
		t.Errorf("public should be untouched, got %v", out["public"])
	}
}

func TestRedactMap(t *testing.T) {
	r := Default()
	in := map[string]any{
		"buyer_email": "bob@x.com",
		"card_number": "4111111111111111",
		"normal_data": "hello",
	}
	out := r.Redact(in).(map[string]any)
	if !strings.Contains(out["buyer_email"].(string), "@x.com") || !strings.HasPrefix(out["buyer_email"].(string), "b***") {
		t.Errorf("buyer_email mask incorrect: %v", out["buyer_email"])
	}
	if out["card_number"] != "411111******1111" {
		t.Errorf("card_number mask incorrect: %v", out["card_number"])
	}
	if out["normal_data"] != "hello" {
		t.Errorf("normal_data should be untouched, got %v", out["normal_data"])
	}
}

func TestRedactString_Luhn(t *testing.T) {
	r := Default()
	in := "txn ok, card=4111111111111111 amt=100"
	out := r.RedactString(in)
	if !strings.Contains(out, "411111******1111") {
		t.Errorf("Luhn scrub should mask card: %q", out)
	}
	// "100" is digit run but not Luhn-valid + too short → passthrough
	if !strings.Contains(out, "amt=100") {
		t.Errorf("non-card digits should pass: %q", out)
	}
}

func TestRedactString_LuhnDisabled(t *testing.T) {
	r := New(DefaultFieldRules, false)
	in := "card=4111111111111111"
	out := r.RedactString(in)
	if out != in {
		t.Errorf("Luhn disabled, expected unchanged: %q", out)
	}
}

func TestNestedStruct(t *testing.T) {
	type Inner struct {
		Pan string `json:"pan"`
	}
	type Outer struct {
		Inner Inner  `json:"inner"`
		Note  string `json:"note"`
	}
	out := Default().Redact(Outer{
		Inner: Inner{Pan: "4111111111111111"},
		Note:  "ok",
	}).(map[string]any)
	inner := out["inner"].(map[string]any)
	if inner["pan"] != "411111******1111" {
		t.Errorf("nested pan should be masked, got %v", inner["pan"])
	}
}

func TestWith(t *testing.T) {
	r := Default().With(FieldRule{Match: "internal_id", Action: ActionDrop})
	type S struct {
		InternalID string `json:"internal_id"`
	}
	out := r.Redact(S{InternalID: "secret123"}).(map[string]any)
	if out["internal_id"] != "<redacted>" {
		t.Errorf("custom rule should apply, got %v", out["internal_id"])
	}
	// original default still has password
	out2 := Default().Redact(S{InternalID: "secret123"}).(map[string]any)
	if out2["internal_id"] == "<redacted>" {
		t.Error("custom rule leaked into default")
	}
}

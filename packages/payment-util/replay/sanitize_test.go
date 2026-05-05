package replay

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMaskPAN(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"4111111111111111", "411111******1111"},
		{"345678901234567", "345678*****4567"},
		{"4242424242424242", "424242******4242"},
		{"1234", "1234"}, // 太短不动
		{"", ""},
	}
	for _, c := range cases {
		if got := maskPAN(c.in); got != c.want {
			t.Errorf("maskPAN(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}

func TestLuhnValid(t *testing.T) {
	if !luhnValid("4111111111111111") {
		t.Fatal("Visa test card should pass Luhn")
	}
	if luhnValid("4111111111111112") {
		t.Fatal("modified Visa should fail Luhn")
	}
	if luhnValid("123456789012") {
		t.Fatal("random 12-digit shouldn't pass Luhn")
	}
}

func TestSanitizer_DeterministicFake_Stable(t *testing.T) {
	s := NewSanitizer([]byte("test-key-32-bytes-aaaaaaaaaaaa"), nil)
	a := s.deterministicFake("alice@example.com")
	b := s.deterministicFake("alice@example.com")
	c := s.deterministicFake("bob@example.com")
	if a != b {
		t.Errorf("deterministic fake不稳定：a=%s b=%s", a, b)
	}
	if a == c {
		t.Errorf("不同输入给了相同 fake：alice=%s bob=%s", a, c)
	}
}

func TestSanitizer_ShadowUserID_InRange(t *testing.T) {
	s := NewSanitizer([]byte("test-key-32-bytes-aaaaaaaaaaaa"), nil)
	for _, real := range []int64{100000001, 100000999, 200000123} {
		fake := s.shadowUserID(real)
		if fake < 9_000_000_000 || fake >= 9_010_000_000 {
			t.Errorf("shadowUserID(%d) = %d 不在 shadow 段 [9e9, 9.01e9)", real, fake)
		}
	}
}

func TestSanitizer_Sanitize_PII(t *testing.T) {
	body := map[string]any{
		"merchant_id": "MERCH_12345",
		"user_id":     "100000001",
		"amount":      1000.0,
		"currency":    "PHP",
		"email":       "alice@example.com",
		"phone":       "+639171234567",
		"card_number": "4111111111111111",
		"cvv":         "123",
	}
	raw, _ := json.Marshal(body)
	s := NewSanitizer([]byte("test-key-32-bytes-aaaaaaaaaaaa"), nil)
	ev := &Event{Method: "/test.M", BodyJSON: raw}
	out, err := s.Sanitize(ev)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.BodyJSON, &got); err != nil {
		t.Fatal(err)
	}
	// CVV 必须删掉
	if v, ok := got["cvv"]; ok && v != nil {
		t.Errorf("cvv should be dropped, got %v", v)
	}
	// PAN 必须 mask
	if pan, _ := got["card_number"].(string); pan != "411111******1111" {
		t.Errorf("card_number should be masked, got %v", pan)
	}
	// email / phone 应该是 hex（deterministicFake）
	if email, _ := got["email"].(string); email == "alice@example.com" {
		t.Errorf("email not faked")
	}
	if phone, _ := got["phone"].(string); phone == "+639171234567" {
		t.Errorf("phone not faked")
	}
	// amount / currency 必须保留
	if amt, _ := got["amount"].(float64); amt != 1000.0 {
		t.Errorf("amount should be preserved, got %v", amt)
	}
	// merchant_id 应该 fake
	if m, _ := got["merchant_id"].(string); m == "MERCH_12345" {
		t.Errorf("merchant_id not faked")
	}
}

func TestScrubFreeTextPAN(t *testing.T) {
	in := "Customer says card 4111111111111111 was charged twice"
	out := ScrubFreeTextPAN(in)
	if strings.Contains(out, "4111111111111111") {
		t.Errorf("free-text scrub failed: %s", out)
	}
	if !strings.Contains(out, "411111******1111") {
		t.Errorf("expected masked, got: %s", out)
	}
}

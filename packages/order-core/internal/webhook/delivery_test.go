package webhook

import (
	"testing"
	"time"
)

func TestSign(t *testing.T) {
	secret := "whsec_test123"
	ts := "1700000000"
	body := []byte(`{"type":"payment_intent.succeeded","id":"pi_xxx"}`)

	sig := sign(secret, ts, body)
	if sig == "" {
		t.Fatal("signature should not be empty")
	}

	// Same inputs → same output (deterministic)
	sig2 := sign(secret, ts, body)
	if sig != sig2 {
		t.Fatal("sign should be deterministic")
	}

	// Different secret → different sig
	sig3 := sign("other_secret", ts, body)
	if sig == sig3 {
		t.Fatal("different secret should produce different signature")
	}
}

func TestVerifySignature_Happy(t *testing.T) {
	secret := "whsec_test123"
	ts := time.Now().Unix()
	body := []byte(`{"hello":"world"}`)

	tsStr := ""
	for n := ts; n > 0; n /= 10 {
		tsStr = string(rune('0'+n%10)) + tsStr
	}
	sig := sign(secret, tsStr, body)
	header := "t=" + tsStr + ",v1=" + sig

	if !VerifySignature(secret, header, body, 5*time.Minute) {
		t.Fatal("valid signature should verify")
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	secret := "whsec_correct"
	body := []byte(`test`)
	ts := "1700000000"
	sig := sign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if VerifySignature("whsec_wrong", header, body, 0) {
		t.Fatal("wrong secret should fail verification")
	}
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`original`)
	ts := "1700000000"
	sig := sign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if VerifySignature(secret, header, []byte(`tampered`), 0) {
		t.Fatal("tampered body should fail verification")
	}
}

func TestVerifySignature_Expired(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`test`)
	ts := "1600000000" // very old
	sig := sign(secret, ts, body)
	header := "t=" + ts + ",v1=" + sig

	if VerifySignature(secret, header, body, 5*time.Minute) {
		t.Fatal("expired timestamp should fail")
	}
}

func TestSplitSig(t *testing.T) {
	parts := splitSig("t=123,v1=abc")
	if len(parts) != 2 || parts[0] != "t=123" || parts[1] != "v1=abc" {
		t.Fatalf("got %v", parts)
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"seconds", "30", 30},
		{"zero rejected", "0", 0},
		{"negative rejected", "-5", 0},
		{"capped at 1h", "100000", 3600},
		{"http-date past", "Mon, 01 Jan 2000 00:00:00 GMT", 0},
		{"garbage", "abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRetryAfter(tt.in); got != tt.want {
				t.Fatalf("parseRetryAfter(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestJitter_StaysWithinBand(t *testing.T) {
	base := 30 * time.Second
	low := time.Duration(float64(base) * 0.74)  // -25% with rounding
	high := time.Duration(float64(base) * 1.26) // +25% with rounding
	for i := 0; i < 1000; i++ {
		got := jitter(base)
		if got < low || got > high {
			t.Fatalf("jitter(%s) = %s outside [%s,%s]", base, got, low, high)
		}
	}
}

func TestJitter_ZeroPassesThrough(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %s, want 0", got)
	}
	neg := -5 * time.Second
	if got := jitter(neg); got != neg {
		t.Fatalf("jitter(-5s) = %s, want -5s (passthrough)", got)
	}
}

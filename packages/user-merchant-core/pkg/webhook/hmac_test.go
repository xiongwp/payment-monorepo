package webhook

import (
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"event":"payment_intent.succeeded"}`)
	now := time.Unix(1_700_000_000, 0)

	sig := SignAt(secret, body, now)
	if err := VerifyAt(secret, body, sig, 5*time.Minute, now); err != nil {
		t.Fatalf("verify same-second: %v", err)
	}
	// within tolerance
	if err := VerifyAt(secret, body, sig, 5*time.Minute, now.Add(60*time.Second)); err != nil {
		t.Fatalf("verify +60s: %v", err)
	}
	// outside tolerance
	if err := VerifyAt(secret, body, sig, 5*time.Minute, now.Add(10*time.Minute)); err == nil {
		t.Fatal("expected error for stale signature")
	}
}

func TestVerifyTamperedBody(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"a":1}`)
	sig := Sign(secret, body)
	if err := Verify(secret, []byte(`{"a":2}`), sig, time.Hour); err == nil {
		t.Fatal("expected error for tampered body")
	}
}

func TestVerifyBadHeader(t *testing.T) {
	if err := Verify([]byte("x"), []byte("y"), "garbage", time.Hour); err == nil {
		t.Fatal("expected error for bad header")
	}
	if err := Verify([]byte("x"), []byte("y"), "t=100", time.Hour); err == nil {
		t.Fatal("expected error for header missing v1")
	}
}

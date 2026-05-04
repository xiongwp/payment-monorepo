package crypto

import (
	"crypto/rand"
	"testing"
)

func TestAESGCMRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	c, err := NewAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("sensitive-secret-123456")
	enc, err := c.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if enc == string(plain) {
		t.Errorf("Seal returned plaintext")
	}
	got, err := c.Open(enc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Errorf("round-trip mismatch: %q vs %q", got, plain)
	}
}

func TestAESGCMRejectBadPayload(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	c, _ := NewAESGCM(key)
	if _, err := c.Open("garbage"); err == nil {
		t.Errorf("expected error on bad payload")
	}
	if _, err := c.Open("v1:not-base64"); err == nil {
		t.Errorf("expected error on non-base64")
	}
}

func TestNoopCipherPassthrough(t *testing.T) {
	var c FieldCipher = NoopCipher{}
	enc, _ := c.Seal([]byte("plain"))
	if enc != "plain" {
		t.Errorf("Noop should pass through")
	}
	dec, _ := c.Open(enc)
	if string(dec) != "plain" {
		t.Errorf("Noop decode mismatch")
	}
}

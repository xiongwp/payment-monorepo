package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestAESGCM_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := NewAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("some-sensitive-token-9876543210")
	enc, err := c.Seal(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) < 4 || enc[:3] != "v1:" {
		t.Fatalf("bad enc prefix: %q", enc[:4])
	}
	out, err := c.Open(enc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plaintext) {
		t.Fatalf("want %q got %q", plaintext, out)
	}
}

func TestAESGCM_OpenRejectsBadInput(t *testing.T) {
	key := make([]byte, 32)
	c, _ := NewAESGCM(key)
	if _, err := c.Open(""); err == nil {
		t.Fatal("want error")
	}
	if _, err := c.Open("v1:aaa"); err == nil {
		t.Fatal("want error on short")
	}
	if _, err := c.Open("v2:xxx"); err == nil {
		t.Fatal("want error on bad version")
	}
}

func TestNoop(t *testing.T) {
	c := NoopCipher{}
	enc, _ := c.Seal([]byte("x"))
	out, _ := c.Open(enc)
	if string(out) != "x" {
		t.Fatalf("noop roundtrip got %q", out)
	}
}

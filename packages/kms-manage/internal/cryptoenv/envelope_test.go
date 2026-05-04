package cryptoenv

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func mustKey(t *testing.T, n int) []byte {
	t.Helper()
	k := make([]byte, n)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip_AllKeySizes(t *testing.T) {
	for _, n := range []int{16, 24, 32} {
		key := mustKey(t, n)
		ct, err := Encrypt(key, "k1", "svc:pc:token", []byte("hello"))
		if err != nil {
			t.Fatalf("encrypt n=%d: %v", n, err)
		}
		if !strings.HasPrefix(ct, "kms:v1:k1:") {
			t.Fatalf("bad envelope prefix: %s", ct)
		}
		plain, kid, err := Decrypt(map[string][]byte{"k1": key}, ct, "svc:pc:token")
		if err != nil {
			t.Fatalf("decrypt n=%d: %v", n, err)
		}
		if kid != "k1" || !bytes.Equal(plain, []byte("hello")) {
			t.Fatalf("round-trip lost: kid=%s plain=%q", kid, plain)
		}
	}
}

func TestWrongContextFails(t *testing.T) {
	key := mustKey(t, 32)
	ct, err := Encrypt(key, "k1", "svc:pc:token", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Decrypt(map[string][]byte{"k1": key}, ct, "svc:pc:other"); err == nil {
		t.Fatal("wrong context must fail AEAD check")
	}
}

func TestWrongKeyFails(t *testing.T) {
	k1 := mustKey(t, 32)
	k2 := mustKey(t, 32)
	ct, _ := Encrypt(k1, "k1", "", []byte("x"))
	// 用错密钥
	if _, _, err := Decrypt(map[string][]byte{"k1": k2}, ct, ""); err == nil {
		t.Fatal("wrong key must fail GCM tag check")
	}
}

func TestUnknownKeyID(t *testing.T) {
	key := mustKey(t, 32)
	ct, _ := Encrypt(key, "rotated", "", []byte("x"))
	if _, _, err := Decrypt(map[string][]byte{"current": key}, ct, ""); err == nil {
		t.Fatal("unknown key_id must be rejected before aes call")
	}
}

func TestParseEnvelope(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		kid string
	}{
		{"kms:v1:main:QUJDREVGR0hJSktM", true, "main"},
		{"kms:v1::abc", false, ""},
		{"kms:v1:abc", false, ""},
		{"kms:v2:main:abc", false, ""},
		{"plainstring", false, ""},
	}
	for _, c := range cases {
		kid, _, err := ParseEnvelope(c.in)
		gotOK := err == nil
		if gotOK != c.ok {
			t.Fatalf("%q: ok=%v want %v (err=%v)", c.in, gotOK, c.ok, err)
		}
		if gotOK && kid != c.kid {
			t.Fatalf("%q: kid=%q want %q", c.in, kid, c.kid)
		}
	}
}

func TestBadKeySize(t *testing.T) {
	if err := ValidateKey(make([]byte, 7)); err == nil {
		t.Fatal("7-byte key must be rejected")
	}
	if _, err := Encrypt(make([]byte, 7), "k", "", []byte{}); err == nil {
		t.Fatal("encrypt with 7-byte key must fail")
	}
}

func TestCiphertextIsStable(t *testing.T) {
	// 同样输入不同 nonce 应该产不同密文，但都能解出原文。
	key := mustKey(t, 32)
	ct1, _ := Encrypt(key, "k", "", []byte("x"))
	ct2, _ := Encrypt(key, "k", "", []byte("x"))
	if ct1 == ct2 {
		t.Fatal("nonce must be random → ciphertexts must differ")
	}
}

func TestIsCiphertext(t *testing.T) {
	if !IsCiphertext("kms:v1:k:AAAA") {
		t.Fatal()
	}
	if IsCiphertext("plain") {
		t.Fatal()
	}
}

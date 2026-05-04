package cryptoenv

import "testing"

func TestGCMCache_HitsForSameKey(t *testing.T) {
	ResetGCMCache()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	g1, err := gcmFor(key)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := gcmFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if g1 != g2 {
		t.Fatal("expected same AEAD instance from cache, got two")
	}
}

func TestGCMCache_DifferentKeysDifferentInstances(t *testing.T) {
	ResetGCMCache()
	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	for i := range k1 {
		k1[i] = byte(i)
		k2[i] = byte(i + 1)
	}
	g1, err := gcmFor(k1)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := gcmFor(k2)
	if err != nil {
		t.Fatal(err)
	}
	if g1 == g2 {
		t.Fatal("different keys should yield different AEAD")
	}
}

func TestGCMCache_ResetForcesRebuild(t *testing.T) {
	ResetGCMCache()
	key := make([]byte, 32)
	g1, err := gcmFor(key)
	if err != nil {
		t.Fatal(err)
	}
	ResetGCMCache()
	g2, err := gcmFor(key)
	if err != nil {
		t.Fatal(err)
	}
	if g1 == g2 {
		t.Fatal("after reset, expected fresh AEAD instance")
	}
}

// 端到端：通过 Encrypt → Decrypt 验证使用 cache 后语义不变（最关键的回归）。
func TestEncryptDecrypt_RoundTripWithCache(t *testing.T) {
	ResetGCMCache()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	keyID := "k1"
	ctxStr := "svc:test:field"
	plain := []byte("super secret")

	ct, err := Encrypt(key, keyID, ctxStr, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, gotKeyID, err := Decrypt(map[string][]byte{keyID: key}, ct, ctxStr)
	if err != nil {
		t.Fatal(err)
	}
	if gotKeyID != keyID {
		t.Fatalf("expected keyID %s got %s", keyID, gotKeyID)
	}
	if string(got) != string(plain) {
		t.Fatalf("expected %q got %q", plain, got)
	}
}

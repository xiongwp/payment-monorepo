package jwks

import (
	"strings"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	ks := NewKeyStore("https://oauth.test", "test-aud")
	if err := ks.LoadOrGenerate(""); err != nil {
		t.Fatalf("gen key: %v", err)
	}
	if ks.Active() == nil {
		t.Fatal("no active key after LoadOrGenerate")
	}

	claims := map[string]any{
		"iss": "https://oauth.test",
		"aud": "test-aud",
		"sub": "client_123",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
		"iat": float64(time.Now().Unix()),
	}
	tok, err := ks.Sign(claims)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if parts := strings.Split(tok, "."); len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d", len(parts))
	}

	got, err := ks.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got["sub"] != "client_123" {
		t.Errorf("sub mismatch: %v", got["sub"])
	}
}

func TestVerifyExpired(t *testing.T) {
	ks := NewKeyStore("iss", "aud")
	_ = ks.LoadOrGenerate("")

	tok, _ := ks.Sign(map[string]any{
		"iss": "iss", "aud": "aud",
		"exp": float64(time.Now().Add(-1 * time.Hour).Unix()), // 已过期
		"iat": float64(time.Now().Add(-2 * time.Hour).Unix()),
	})
	_, err := ks.Verify(tok)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expected expired error, got %v", err)
	}
}

func TestVerifyWrongIssuer(t *testing.T) {
	ks := NewKeyStore("iss-A", "aud")
	_ = ks.LoadOrGenerate("")

	tok, _ := ks.Sign(map[string]any{
		"iss": "iss-B", "aud": "aud", // 错的 issuer
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	_, err := ks.Verify(tok)
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Errorf("expected issuer mismatch, got %v", err)
	}
}

func TestVerifyWrongAudience(t *testing.T) {
	ks := NewKeyStore("iss", "aud-A")
	_ = ks.LoadOrGenerate("")
	tok, _ := ks.Sign(map[string]any{
		"iss": "iss", "aud": "aud-B",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	_, err := ks.Verify(tok)
	if err == nil || !strings.Contains(err.Error(), "audience") {
		t.Errorf("expected audience mismatch, got %v", err)
	}
}

func TestRotate(t *testing.T) {
	ks := NewKeyStore("iss", "aud")
	_ = ks.LoadOrGenerate("")
	oldKID := ks.Active().KID

	// 用老 key 签
	oldTok, _ := ks.Sign(map[string]any{
		"iss": "iss", "aud": "aud",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})

	// rotate
	if err := ks.Rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	newKID := ks.Active().KID
	if oldKID == newKID {
		t.Fatal("rotate did not change kid")
	}

	// 老 token 仍可验 (在 retired 池)
	if _, err := ks.Verify(oldTok); err != nil {
		t.Errorf("retired key verify failed: %v", err)
	}

	// 新签 token 用新 kid
	newTok, _ := ks.Sign(map[string]any{
		"iss": "iss", "aud": "aud",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	if newTok == oldTok {
		t.Fatal("new token same as old")
	}
}

func TestPurgeRetired(t *testing.T) {
	ks := NewKeyStore("iss", "aud")
	_ = ks.LoadOrGenerate("")
	_ = ks.Rotate()

	// 把 retired 的 CreatedAt 调到 35 天前，触发 purge
	for _, kp := range ks.retired {
		kp.CreatedAt = time.Now().Add(-35 * 24 * time.Hour)
	}
	n := ks.PurgeRetired()
	if n != 1 {
		t.Errorf("expected 1 purge, got %d", n)
	}
	if len(ks.retired) != 0 {
		t.Errorf("retired not empty after purge: %d", len(ks.retired))
	}
}

func TestVerifyTamperedSignature(t *testing.T) {
	ks := NewKeyStore("iss", "aud")
	_ = ks.LoadOrGenerate("")
	tok, _ := ks.Sign(map[string]any{
		"iss": "iss", "aud": "aud",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	// 篡改最后 1 字符 (sig 部分)
	tampered := tok[:len(tok)-2] + "AA"
	_, err := ks.Verify(tampered)
	if err == nil {
		t.Fatal("tampered token verified")
	}
}

func TestVerifyTamperedClaims(t *testing.T) {
	ks := NewKeyStore("iss", "aud")
	_ = ks.LoadOrGenerate("")
	tok, _ := ks.Sign(map[string]any{
		"iss": "iss", "aud": "aud", "sub": "real-user",
		"exp": float64(time.Now().Add(time.Hour).Unix()),
	})
	parts := strings.SplitN(tok, ".", 3)
	// 替换 claims 部分 = 改 sub 但 sig 不动
	parts[1] = "eyJzdWIiOiJoYWNrZXIifQ" // base64url({"sub":"hacker"})
	tampered := parts[0] + "." + parts[1] + "." + parts[2]
	_, err := ks.Verify(tampered)
	if err == nil {
		t.Fatal("tampered claims verified")
	}
}

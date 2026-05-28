package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha256"
	_ "crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// 测试用 mock IdP：进程内 RSA 私钥签 token，让 OIDCProvider.Verify 校验。
// 避免依赖真 IdP（Keycloak / Google）+ 不需引第三方 JWT 库。

type mockIdP struct {
	priv     *rsa.PrivateKey
	kid      string
	issuer   string
	clientID string
}

func newMockIdP(t *testing.T) *mockIdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &mockIdP{priv: k, kid: "test-kid", issuer: "https://idp.test", clientID: "risk-admin"}
}

func (m *mockIdP) provider() *OIDCProvider {
	// 直接构造已预热 cache 的 provider，跳过 discovery。
	p := &OIDCProvider{
		IssuerURL:  m.issuer,
		ClientID:   m.clientID,
		cachedKeys: map[string]*rsa.PublicKey{m.kid: &m.priv.PublicKey},
		cachedAt:   time.Now(),
	}
	return p
}

func (m *mockIdP) sign(t *testing.T, claims map[string]any, alg string) string {
	t.Helper()
	if alg == "" {
		alg = "RS256"
	}
	hdr := map[string]string{"alg": alg, "kid": m.kid, "typ": "JWT"}
	hb, _ := json.Marshal(hdr)
	pb, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(hb) + "." + enc.EncodeToString(pb)
	sig := signJWT(t, m.priv, alg, []byte(signingInput))
	return signingInput + "." + enc.EncodeToString(sig)
}

func signJWT(t *testing.T, priv *rsa.PrivateKey, alg string, data []byte) []byte {
	t.Helper()
	var h crypto.Hash
	switch alg {
	case "RS256":
		h = crypto.SHA256
	case "RS384":
		h = crypto.SHA384
	case "RS512":
		h = crypto.SHA512
	default:
		t.Fatalf("unsupported alg %s", alg)
	}
	hasher := h.New()
	hasher.Write(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, h, hasher.Sum(nil))
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func baseClaims(idp *mockIdP) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss":    idp.issuer,
		"aud":    idp.clientID,
		"sub":    "user-1",
		"email":  "alice@example.com",
		"name":   "Alice",
		"groups": []string{"risk-admin"},
		"iat":    now,
		"exp":    now + 3600,
	}
}

func TestOIDC_VerifyHappyPath(t *testing.T) {
	idp := newMockIdP(t)
	p := idp.provider()
	tok := idp.sign(t, baseClaims(idp), "RS256")
	c, err := p.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.Subject != "user-1" || c.Email != "alice@example.com" {
		t.Fatalf("claims wrong: %+v", c)
	}
	if len(c.Groups) != 1 || c.Groups[0] != "risk-admin" {
		t.Fatalf("groups wrong: %v", c.Groups)
	}
}

func TestOIDC_Expired(t *testing.T) {
	idp := newMockIdP(t)
	p := idp.provider()
	cl := baseClaims(idp)
	cl["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	tok := idp.sign(t, cl, "RS256")
	if _, err := p.Verify(context.Background(), tok); err == nil {
		t.Fatal("expected expired error")
	}
}

func TestOIDC_BadSignature(t *testing.T) {
	idp := newMockIdP(t)
	p := idp.provider()
	tok := idp.sign(t, baseClaims(idp), "RS256")
	// flip 最后一位
	tampered := tok[:len(tok)-2] + "AA"
	if _, err := p.Verify(context.Background(), tampered); err == nil {
		t.Fatal("expected bad signature")
	}
}

func TestOIDC_MissingAud(t *testing.T) {
	idp := newMockIdP(t)
	p := idp.provider()
	cl := baseClaims(idp)
	cl["aud"] = "other-client"
	tok := idp.sign(t, cl, "RS256")
	if _, err := p.Verify(context.Background(), tok); err == nil {
		t.Fatal("expected aud mismatch")
	}
}

func TestOIDC_WrongIssuer(t *testing.T) {
	idp := newMockIdP(t)
	p := idp.provider()
	cl := baseClaims(idp)
	cl["iss"] = "https://attacker.test"
	tok := idp.sign(t, cl, "RS256")
	if _, err := p.Verify(context.Background(), tok); err == nil {
		t.Fatal("expected iss mismatch")
	}
}

// ─── TOTP ────────────────────────────────────────────────────────────────

func TestTOTP_CorrectCode(t *testing.T) {
	v := NewTOTPVerifier()
	secret, _, err := v.Enroll("u1", "RiskAdmin", "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	code, _ := GenerateAt(secret, time.Now())
	if !v.Verify("u1", code) {
		t.Fatal("expected verify ok")
	}
}

func TestTOTP_OneStepEarlier(t *testing.T) {
	v := NewTOTPVerifier()
	secret, _, _ := v.Enroll("u1", "R", "a")
	code, _ := GenerateAt(secret, time.Now().Add(-30*time.Second))
	if !v.Verify("u1", code) {
		t.Fatal("expected -1 step accepted")
	}
}

func TestTOTP_OneStepLater(t *testing.T) {
	v := NewTOTPVerifier()
	secret, _, _ := v.Enroll("u1", "R", "a")
	code, _ := GenerateAt(secret, time.Now().Add(30*time.Second))
	if !v.Verify("u1", code) {
		t.Fatal("expected +1 step accepted")
	}
}

func TestTOTP_WrongCode(t *testing.T) {
	v := NewTOTPVerifier()
	_, _, _ = v.Enroll("u1", "R", "a")
	if v.Verify("u1", "000000") {
		t.Fatal("expected wrong code rejected")
	}
}

func TestTOTP_Replay(t *testing.T) {
	v := NewTOTPVerifier()
	secret, _, _ := v.Enroll("u1", "R", "a")
	code, _ := GenerateAt(secret, time.Now())
	if !v.Verify("u1", code) {
		t.Fatal("first verify should pass")
	}
	if v.Verify("u1", code) {
		t.Fatal("second verify with same code should be rejected (replay)")
	}
}


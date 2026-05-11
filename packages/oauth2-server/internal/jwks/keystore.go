// Package jwks — RSA key 管理 + JWKS endpoint (/.well-known/jwks.json)。
//
// 用法:
//   1. 启动期 LoadOrGenerate() 加载已有 key 或生成新的（key rotation 时切）
//   2. SignClaim(claims) 签 RS256 JWT (kid 是 key fingerprint)
//   3. Verify(token) 检签 + 检 exp/iss/aud
//   4. ServeJWKS(w, r) 暴露 /.well-known/jwks.json 给客户端 fetch
//
// Key rotation:
//   - 每 90 天生成新 key，旧 key 保留 30 天供验签老 token
//   - cert-manager 集成签发 / 自动 rotate
//
// 标准:
//   RFC 7519 JWT
//   RFC 7517 JWK Set
//   RFC 7518 JWA (RS256)

package jwks

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// KeyStore 持 active + retired key pairs。
type KeyStore struct {
	mu       sync.RWMutex
	active   *KeyPair          // 签新 token 用
	retired  map[string]*KeyPair // 老 token 验签用 (key by kid)
	issuer   string
	audience string
}

// KeyPair RSA key + kid。
type KeyPair struct {
	KID        string
	PrivateKey *rsa.PrivateKey
	PublicKey  *rsa.PublicKey
	CreatedAt  time.Time
}

// NewKeyStore 构造。
func NewKeyStore(issuer, audience string) *KeyStore {
	return &KeyStore{
		retired:  map[string]*KeyPair{},
		issuer:   issuer,
		audience: audience,
	}
}

// LoadOrGenerate 从 path 加载 RSA private key；不存在则生成新的。
func (ks *KeyStore) LoadOrGenerate(privPath string) error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if privPath != "" {
		if body, err := os.ReadFile(privPath); err == nil {
			kp, err := decodePEM(body)
			if err == nil {
				ks.active = kp
				return nil
			}
		}
	}
	// 生成新 key
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	kid := fingerprint(&priv.PublicKey)
	ks.active = &KeyPair{
		KID: kid, PrivateKey: priv, PublicKey: &priv.PublicKey,
		CreatedAt: time.Now().UTC(),
	}
	// persist
	if privPath != "" {
		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(priv),
		})
		_ = os.WriteFile(privPath, pemBytes, 0600)
	}
	return nil
}

// Rotate 生成新 active key，把旧的移到 retired (验签仍可用 30d)。
func (ks *KeyStore) Rotate() error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	kid := fingerprint(&priv.PublicKey)
	if ks.active != nil {
		ks.retired[ks.active.KID] = ks.active
	}
	ks.active = &KeyPair{
		KID: kid, PrivateKey: priv, PublicKey: &priv.PublicKey,
		CreatedAt: time.Now().UTC(),
	}
	return nil
}

// PurgeRetired 清掉 30d+ 的 retired key（不再支持验签）。
func (ks *KeyStore) PurgeRetired() int {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	n := 0
	for kid, kp := range ks.retired {
		if kp.CreatedAt.Before(cutoff) {
			delete(ks.retired, kid)
			n++
		}
	}
	return n
}

// findKey kid → KeyPair (active 或 retired)。
func (ks *KeyStore) findKey(kid string) *KeyPair {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	if ks.active != nil && ks.active.KID == kid {
		return ks.active
	}
	return ks.retired[kid]
}

// Active 返当前签 token 用的 key。
func (ks *KeyStore) Active() *KeyPair {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.active
}

// Sign 签 RS256 JWT。claims 序列化成 JSON 当 payload。
func (ks *KeyStore) Sign(claims map[string]any) (string, error) {
	kp := ks.Active()
	if kp == nil {
		return "", errors.New("no active key")
	}
	header := map[string]any{
		"alg": "RS256",
		"typ": "JWT",
		"kid": kp.KID,
	}
	headerJSON, _ := json.Marshal(header)
	claimsJSON, _ := json.Marshal(claims)
	signing := b64(headerJSON) + "." + b64(claimsJSON)
	sig, err := signRS256(kp.PrivateKey, []byte(signing))
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// Verify 解析 + 验签 + 检 exp/iss/aud。
func (ks *KeyStore) Verify(token string) (map[string]any, error) {
	parts := splitN(token, ".", 3)
	if len(parts) != 3 {
		return nil, errors.New("invalid token format")
	}
	headerBytes, err := b64decode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	if header["alg"] != "RS256" {
		return nil, errors.New("only RS256 supported")
	}
	kid, _ := header["kid"].(string)
	kp := ks.findKey(kid)
	if kp == nil {
		return nil, fmt.Errorf("unknown key id %q", kid)
	}
	sigBytes, err := b64decode(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode sig: %w", err)
	}
	signing := parts[0] + "." + parts[1]
	if err := verifyRS256(kp.PublicKey, []byte(signing), sigBytes); err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	claimsBytes, err := b64decode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	// 校验 exp
	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().Unix() > int64(exp) {
			return nil, errors.New("token expired")
		}
	}
	// 校验 iss / aud
	if ks.issuer != "" {
		if iss, ok := claims["iss"].(string); !ok || iss != ks.issuer {
			return nil, fmt.Errorf("issuer mismatch")
		}
	}
	if ks.audience != "" {
		if aud, ok := claims["aud"].(string); !ok || aud != ks.audience {
			return nil, fmt.Errorf("audience mismatch")
		}
	}
	return claims, nil
}

// ServeJWKS 暴露 /.well-known/jwks.json — 客户端 fetch 后本地缓存验签。
//
// 返 active + retired 全部公钥（让旧 token 也能验）。
func (ks *KeyStore) ServeJWKS(w http.ResponseWriter, r *http.Request) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	keys := []map[string]any{}
	addKey := func(kp *KeyPair) {
		n := kp.PublicKey.N
		e := kp.PublicKey.E
		keys = append(keys, map[string]any{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": kp.KID,
			"n":   b64(n.Bytes()),
			"e":   b64(intToBytes(e)),
		})
	}
	if ks.active != nil {
		addKey(ks.active)
	}
	for _, kp := range ks.retired {
		addKey(kp)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	json.NewEncoder(w).Encode(map[string]any{"keys": keys})
}

// ─── helpers ────────────────────────────────────────────────────────

func fingerprint(pub *rsa.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	sum := sha256.Sum256(der)
	return b64(sum[:8])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func b64decode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

func splitN(s, sep string, n int) []string {
	out := []string{}
	for i := 0; i < n-1; i++ {
		idx := indexOf(s, sep)
		if idx < 0 {
			break
		}
		out = append(out, s[:idx])
		s = s[idx+len(sep):]
	}
	out = append(out, s)
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func intToBytes(n int) []byte {
	if n < 256 {
		return []byte{byte(n)}
	}
	return []byte{byte(n >> 16), byte(n >> 8), byte(n)}
}

func decodePEM(body []byte) (*KeyPair, error) {
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("invalid PEM")
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	kid := fingerprint(&priv.PublicKey)
	return &KeyPair{
		KID: kid, PrivateKey: priv, PublicKey: &priv.PublicKey,
		CreatedAt: time.Now().UTC(),
	}, nil
}

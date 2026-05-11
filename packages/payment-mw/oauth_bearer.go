// oauth_bearer.go — OAuth2 Bearer JWT 验签 (RS256) + JWKS 拉取缓存。
//
// 用法 (resource server 集成):
//
//   v := mw.NewJWTVerifier(mw.JWTVerifierConfig{
//       JWKSURL:  "https://oauth.payment.local/.well-known/jwks.json",
//       Issuer:   "https://oauth.payment.local",
//       Audience: "payment-api",
//       Refresh:  10 * time.Minute,
//   })
//   chain := mw.Chain(mw.Auth(mw.AuthConfig{
//       PublicPaths: []string{"/healthz"},
//       BearerJWT:   v,                  // 新增
//       MerchantKeys: map[string]string{...},
//   }))
//
// 流程:
//   1. 启动期 fetchJWKS — 失败时报警，依旧启动 (lazy retry)
//   2. 每个请求: 看 Authorization: Bearer <jwt>
//      - 解 header.kid → 查 cache 找公钥
//      - 不存在则刷一次 JWKS（防 key rotation）
//      - 验签 + 检 exp/iss/aud
//   3. 后台 ticker 每 Refresh 周期主动拉一次 (防 cache miss 风暴)
//
// scope → RBAC:
//   actor.Scopes = strings.Split(claims["scope"], " ")
//   业务代码: if !mw.HasScope(ctx, "charge:write") { 403 }
//
// 标准:
//   RFC 7519 JWT
//   RFC 7517 JWK Set
//   RFC 8725 JWT BCP (alg whitelist / kid required)

package mw

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// JWTVerifierConfig 配置。
type JWTVerifierConfig struct {
	JWKSURL  string        // /.well-known/jwks.json
	Issuer   string        // 必须匹配 claim iss
	Audience string        // 必须匹配 claim aud
	Refresh  time.Duration // 后台刷新周期（默认 10min）
	HTTP     *http.Client
	Log      *zap.Logger
}

// JWTVerifier RS256 验签器。线程安全。
type JWTVerifier struct {
	cfg     JWTVerifierConfig
	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey // kid → public key
	lastErr error
	stop    chan struct{}
}

// NewJWTVerifier 构造 + 起后台 refresh。
func NewJWTVerifier(cfg JWTVerifierConfig) *JWTVerifier {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if cfg.Refresh == 0 {
		cfg.Refresh = 10 * time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = zap.NewNop()
	}
	v := &JWTVerifier{
		cfg:  cfg,
		keys: map[string]*rsa.PublicKey{},
		stop: make(chan struct{}),
	}
	if err := v.refresh(); err != nil {
		cfg.Log.Warn("initial JWKS fetch failed (will retry)", zap.Error(err))
	}
	go v.refreshLoop()
	return v
}

// Stop 关后台 refresh。
func (v *JWTVerifier) Stop() { close(v.stop) }

func (v *JWTVerifier) refreshLoop() {
	t := time.NewTicker(v.cfg.Refresh)
	defer t.Stop()
	for {
		select {
		case <-v.stop:
			return
		case <-t.C:
			if err := v.refresh(); err != nil {
				v.cfg.Log.Warn("JWKS refresh failed", zap.Error(err))
			}
		}
	}
}

func (v *JWTVerifier) refresh() error {
	req, _ := http.NewRequest(http.MethodGet, v.cfg.JWKSURL, nil)
	resp, err := v.cfg.HTTP.Do(req)
	if err != nil {
		v.mu.Lock()
		v.lastErr = err
		v.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("jwks fetch status %d", resp.StatusCode)
		v.mu.Lock()
		v.lastErr = err
		v.mu.Unlock()
		return err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("parse jwks: %w", err)
	}
	next := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Alg != "RS256" {
			continue
		}
		nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		switch len(eBytes) {
		case 1:
			e = int(eBytes[0])
		case 2:
			e = int(binary.BigEndian.Uint16(eBytes))
		case 3:
			e = int(eBytes[0])<<16 | int(eBytes[1])<<8 | int(eBytes[2])
		case 4:
			e = int(binary.BigEndian.Uint32(eBytes))
		default:
			continue
		}
		next[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: e,
		}
	}
	v.mu.Lock()
	v.keys = next
	v.lastErr = nil
	v.mu.Unlock()
	v.cfg.Log.Debug("JWKS refreshed", zap.Int("keys", len(next)))
	return nil
}

// Verify 验签 + claim 校验，返 claims map。
func (v *JWTVerifier) Verify(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid jwt format")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, err
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("alg %q not allowed", hdr.Alg)
	}
	if hdr.Kid == "" {
		return nil, errors.New("kid required")
	}

	v.mu.RLock()
	pub, ok := v.keys[hdr.Kid]
	v.mu.RUnlock()
	if !ok {
		// 可能 key rotation 了，强制刷一次
		_ = v.refresh()
		v.mu.RLock()
		pub, ok = v.keys[hdr.Kid]
		v.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", hdr.Kid)
		}
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	signing := parts[0] + "." + parts[1]
	h := sha256.Sum256([]byte(signing))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sigBytes); err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, err
	}

	// exp
	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().Unix() > int64(exp) {
			return nil, errors.New("token expired")
		}
	}
	// iss
	if v.cfg.Issuer != "" {
		if iss, _ := claims["iss"].(string); iss != v.cfg.Issuer {
			return nil, fmt.Errorf("iss mismatch: %s", iss)
		}
	}
	// aud
	if v.cfg.Audience != "" {
		if aud, _ := claims["aud"].(string); aud != v.cfg.Audience {
			return nil, fmt.Errorf("aud mismatch: %s", aud)
		}
	}
	return claims, nil
}

// ─── ctx 辅助 ─────────────────────────────────────────────────────────

// HasScope 检查当前 actor 是否有 scope。
func HasScope(ctx context.Context, want string) bool {
	a := ActorFromCtx(ctx)
	for _, s := range a.Scopes {
		if s == want {
			return true
		}
	}
	return false
}

// RequireScope 中间件 — 如果 actor 没 scope 直接 403。
func RequireScope(want string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !HasScope(r.Context(), want) {
				http.Error(w, "forbidden: missing scope "+want, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

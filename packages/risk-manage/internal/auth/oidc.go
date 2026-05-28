// Package auth — OIDC ID Token verifier。
//
// 设计原则：
//   - 0 第三方依赖（go.mod 没 go-oidc / golang-jwt；新加依赖要走 vendor / security
//     review 流程，本任务范围内不引）。stdlib 的 crypto/rsa + encoding/base64 +
//     encoding/json 拼 RFC 7519 + RFC 7517（JWS / JWK）足够，缺点是只支持
//     RS256 / RS384 / RS512（绝大多数 IdP 默认），HS / ES / EdDSA 留 TODO。
//   - JWKS 远端拉取 10min 缓存：避免每次请求都打 IdP；rotation 时窗内允许重叠。
//     kid mismatch 时强制刷新一次（处理 IdP 即时轮换）。
//   - 校验路径：split JWT → base64url decode header / payload → 拿 alg+kid 找 JWK
//     → 验签 → 校验 exp/iat/nbf/aud/iss。任何一步失败 → ErrInvalidToken。
//   - 时钟漂移容忍 ±60s（行业惯例；OIDC core §15.4 推荐 ≤ 5min，我们更紧）。
//
// 集成进 OIDCMiddleware（同包），不直接接管 admin token map：
// commercial.AdminAuthRoles 保留兼容（feature flag 切；见 middleware.go）。
package auth

import (
	"context"
	"crypto"
	"crypto/rsa"
	_ "crypto/sha256" // register sha256 alg for crypto.Hash.New() used by RS256
	_ "crypto/sha512" // register sha384/512 algs for RS384/RS512
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// ErrInvalidToken OIDC ID Token 校验失败（签名 / claim / exp 任意一项）。
var ErrInvalidToken = errors.New("auth: invalid id token")

// Claims OIDC ID Token 解析出的标准 claim 子集。
type Claims struct {
	Subject string   `json:"sub"`    // 稳定 user id（admin audit 用）
	Email   string   `json:"email"`  // 邮箱（display + audit）
	Name    string   `json:"name"`   // 显示名
	Groups  []string `json:"groups"` // 自定义 claim：admin / oncall / readonly
	Issuer  string   `json:"iss"`
	Audience claimAudience `json:"aud"`
	Expiry  int64    `json:"exp"`
	IssuedAt int64   `json:"iat"`
	NotBefore int64  `json:"nbf"`
}

// claimAudience 处理 aud 可以是 string 或 []string 的二态（OIDC core §2）。
type claimAudience []string

func (a *claimAudience) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*a = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*a = []string{s}
	return nil
}

// jwk JSON Web Key（RFC 7517 §4）。只解 RSA 公钥需要的字段。
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"` // modulus, base64url
	E   string `json:"e"` // exponent, base64url
}

type jwksDoc struct {
	Keys []jwk `json:"keys"`
}

type discoveryDoc struct {
	JWKSURI string `json:"jwks_uri"`
	Issuer  string `json:"issuer"`
}

// OIDCProvider 持有 issuer / client_id / 远端 JWKS 缓存。
type OIDCProvider struct {
	IssuerURL string
	ClientID  string
	jwksURL   string

	mu         sync.RWMutex
	cachedKeys map[string]*rsa.PublicKey // kid → key
	cachedAt   time.Time
	httpClient *http.Client
}

// JWKSCacheTTL JWKS 缓存生命周期；过期后下次 Verify 触发刷新。
const JWKSCacheTTL = 10 * time.Minute

// NewOIDCProvider 拉 discovery 文档 → 拿 jwks_uri → 预热缓存。
// 失败返 error（启动期 fail-fast；IdP 暂时不可达建议外层重试 / fallback bearer）。
func NewOIDCProvider(ctx context.Context, issuer, clientID string) (*OIDCProvider, error) {
	if issuer == "" || clientID == "" {
		return nil, fmt.Errorf("auth: issuer and clientID required")
	}
	p := &OIDCProvider{
		IssuerURL:  issuer,
		ClientID:   clientID,
		cachedKeys: map[string]*rsa.PublicKey{},
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	disco, err := p.fetchDiscovery(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: discovery failed: %w", err)
	}
	if disco.Issuer != "" && disco.Issuer != issuer {
		return nil, fmt.Errorf("auth: discovery issuer mismatch: %q vs %q", disco.Issuer, issuer)
	}
	p.jwksURL = disco.JWKSURI
	if err := p.refreshKeys(ctx); err != nil {
		return nil, fmt.Errorf("auth: prefetch jwks failed: %w", err)
	}
	return p, nil
}

func (p *OIDCProvider) fetchDiscovery(ctx context.Context) (*discoveryDoc, error) {
	url := p.IssuerURL + "/.well-known/openid-configuration"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var d discoveryDoc
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (p *OIDCProvider) refreshKeys(ctx context.Context) error {
	if p.jwksURL == "" {
		return errors.New("jwks_uri empty")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.jwksURL, nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var doc jwksDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue // TODO: ES256 / EdDSA
		}
		pub, err := jwkToRSA(k)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("no usable RSA keys in jwks")
	}
	p.mu.Lock()
	p.cachedKeys = keys
	p.cachedAt = time.Now()
	p.mu.Unlock()
	return nil
}

func jwkToRSA(k jwk) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

func (p *OIDCProvider) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.RLock()
	k, ok := p.cachedKeys[kid]
	stale := time.Since(p.cachedAt) > JWKSCacheTTL
	p.mu.RUnlock()
	if ok && !stale {
		return k, nil
	}
	// kid miss or stale → 强制刷新（throttle 留 future）
	if err := p.refreshKeys(ctx); err != nil {
		if ok {
			return k, nil // 退化用旧 key，避免 IdP 抖动整个 admin 挂
		}
		return nil, err
	}
	p.mu.RLock()
	k = p.cachedKeys[kid]
	p.mu.RUnlock()
	if k == nil {
		return nil, fmt.Errorf("unknown kid: %s", kid)
	}
	return k, nil
}

// Verify 解析 + 验签 + 校验标准 claim。skew 容忍 60s。
func (p *OIDCProvider) Verify(ctx context.Context, idToken string) (*Claims, error) {
	parts := splitJWT(idToken)
	if len(parts) != 3 {
		return nil, ErrInvalidToken
	}
	headerB, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrInvalidToken
	}
	payloadB, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	sigB, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrInvalidToken
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerB, &hdr); err != nil {
		return nil, ErrInvalidToken
	}
	if !isSupportedRSAAlg(hdr.Alg) {
		return nil, fmt.Errorf("%w: unsupported alg %s", ErrInvalidToken, hdr.Alg)
	}
	pub, err := p.keyFor(ctx, hdr.Kid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	if err := verifyRSASignature(hdr.Alg, pub, signingInput, sigB); err != nil {
		return nil, fmt.Errorf("%w: bad signature", ErrInvalidToken)
	}
	var c Claims
	if err := json.Unmarshal(payloadB, &c); err != nil {
		return nil, ErrInvalidToken
	}
	now := time.Now().Unix()
	const skew = 60
	if c.Expiry == 0 || now-skew > c.Expiry {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if c.IssuedAt != 0 && c.IssuedAt > now+skew {
		return nil, fmt.Errorf("%w: iat in future", ErrInvalidToken)
	}
	if c.NotBefore != 0 && c.NotBefore > now+skew {
		return nil, fmt.Errorf("%w: nbf in future", ErrInvalidToken)
	}
	if c.Issuer != p.IssuerURL {
		return nil, fmt.Errorf("%w: bad iss %q", ErrInvalidToken, c.Issuer)
	}
	if len(c.Audience) == 0 {
		return nil, fmt.Errorf("%w: missing aud", ErrInvalidToken)
	}
	audOK := false
	for _, a := range c.Audience {
		if a == p.ClientID {
			audOK = true
			break
		}
	}
	if !audOK {
		return nil, fmt.Errorf("%w: aud mismatch", ErrInvalidToken)
	}
	return &c, nil
}

func splitJWT(s string) []string {
	// 不用 strings.Split 防 0-alloc，JWT 必然 3 段
	out := make([]string, 0, 3)
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func isSupportedRSAAlg(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512":
		return true
	}
	return false
}

func verifyRSASignature(alg string, pub *rsa.PublicKey, signingInput, sig []byte) error {
	var h crypto.Hash
	switch alg {
	case "RS256":
		h = crypto.SHA256
	case "RS384":
		h = crypto.SHA384
	case "RS512":
		h = crypto.SHA512
	default:
		return fmt.Errorf("unsupported alg %s", alg)
	}
	hasher := h.New()
	hasher.Write(signingInput)
	return rsa.VerifyPKCS1v15(pub, h, hasher.Sum(nil), sig)
}

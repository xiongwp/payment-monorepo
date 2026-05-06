// Package authpkg JWT issue / verify + bcrypt + TOTP helpers。
//
// JWT 设计：
//   - 算法：dev=HS256（共享 secret 配置），prod=RS256（私钥签 + 公钥验，KMS-backed
//     私钥更佳）；通过 IssuerConfig.Algorithm 选
//   - Issuer.Verify 严格校验 token.alg == 当前配置 algorithm，拒绝 alg=none 等降级攻击
//   - kid（key id）写入 token header，给未来 key rotation 留 hook（多 key 同时存活）
//   - Token TTL 默认 24h；refresh-token 路径暂不实现
//
// 生产纪律：
//   - assertProdSafety 强制 env=prod 必须 algorithm=RS256（HS256 共享密钥泄漏 = 全站伪造）
//   - 公钥可以散到 verify-only 服务（card-center、order-core、payment-* 都可独立验签
//     而不持私钥）
package authpkg

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// Algorithm JWT 签名算法
type Algorithm string

const (
	AlgHS256 Algorithm = "HS256" // dev 共享密钥
	AlgRS256 Algorithm = "RS256" // prod 私钥签 + 公钥验
)

// Claims 写到 JWT 的 claims。
type Claims struct {
	UserID        string   `json:"sub"`
	EmailVerified bool     `json:"email_verified"`
	Scopes        []string `json:"scopes,omitempty"`
	jwt.RegisteredClaims
}

// IssuerConfig Issuer 装配参数。
//
// HS256 模式：填 Secret；RS256 模式：填 PrivateKeyPEMPath / PublicKeyPEMPath。
// KID 写入 token header，配合 RotationKeys 支持轮换。
//
// **Key rotation 用法**（RS256）：
//   - 启用新 key K2：先把 K2 的公钥加到 RotationKeys（kid="K2" → pub），保持
//     PrivateKeyPEMPath/KID 仍然指 K1。此时 K1 签的 token 用 K1 公钥验，K2 签
//     的（外部签发或灰度）用 K2 公钥验。
//   - 切签发：把 PrivateKeyPEMPath 指 K2，KID="K2"；K1 公钥保留在 RotationKeys
//     里，让 K1 签发的旧 token 在 TTL 窗内仍能验证。
//   - 退役 K1：等所有 K1 签的 token 都过期（默认 TTL=24h，等 25h+）后，从
//     RotationKeys 删 K1，K1 私钥销毁。
type IssuerConfig struct {
	Algorithm         Algorithm
	TTL               time.Duration
	IssuerName        string // 默认 "user-merchant-core"
	KID               string // 当前签发用的 key id

	// HS256 only
	Secret string

	// RS256 only
	PrivateKeyPEMPath string
	PublicKeyPEMPath  string

	// RotationKeys 验签时的多 key 兜底：kid → 公钥 PEM 路径。当前 KID 自动
	// 包含，调用方只需列旧 / 灰度的额外 kid。RS256 才用，HS256 忽略。
	RotationKeys map[string]string
}

// Issuer 配 secret/key + 默认 ttl 后给所有用户签 token。
type Issuer struct {
	alg        Algorithm
	ttl        time.Duration
	issuer     string
	kid        string

	hsSecret []byte // HS256

	rsaPriv     *rsa.PrivateKey            // RS256 sign（当前 active）
	rsaPub      *rsa.PublicKey             // RS256 verify（当前 active 公钥；冗余存储以兼容老路径）
	rsaPubByKid map[string]*rsa.PublicKey  // RS256 verify：kid → 公钥（含 rotation 旧 key）
}

// NewIssuer 旧 API 兼容（HS256 + secret）；保留给已有调用方。
// 新代码用 NewIssuerFromConfig。
func NewIssuer(secret string, ttl time.Duration) *Issuer {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Issuer{
		alg:      AlgHS256,
		ttl:      ttl,
		issuer:   "user-merchant-core",
		kid:      "hs256-default",
		hsSecret: []byte(secret),
	}
}

// NewIssuerFromConfig 完整构造；推荐 prod 用。
func NewIssuerFromConfig(cfg IssuerConfig) (*Issuer, error) {
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	if cfg.IssuerName == "" {
		cfg.IssuerName = "user-merchant-core"
	}
	switch cfg.Algorithm {
	case AlgHS256, "":
		if cfg.Secret == "" {
			return nil, errors.New("authpkg: HS256 requires non-empty Secret")
		}
		return &Issuer{
			alg:      AlgHS256,
			ttl:      cfg.TTL,
			issuer:   cfg.IssuerName,
			kid:      orDefault(cfg.KID, "hs256-default"),
			hsSecret: []byte(cfg.Secret),
		}, nil
	case AlgRS256:
		priv, err := loadRSAPrivate(cfg.PrivateKeyPEMPath)
		if err != nil {
			return nil, fmt.Errorf("authpkg: load private key: %w", err)
		}
		pub, err := loadRSAPublic(cfg.PublicKeyPEMPath)
		if err != nil {
			return nil, fmt.Errorf("authpkg: load public key: %w", err)
		}
		curKid := orDefault(cfg.KID, "rs256-default")
		// kid → pub map：当前 active 自动占一个 slot，rotation extras 加进来。
		// 启动期一次性加载，验签热路径只做 map lookup。
		pubByKid := map[string]*rsa.PublicKey{curKid: pub}
		for kid, path := range cfg.RotationKeys {
			if kid == "" || path == "" || kid == curKid {
				continue // 跳空 / 跳与 active 同 kid 的覆盖
			}
			p, err := loadRSAPublic(path)
			if err != nil {
				return nil, fmt.Errorf("authpkg: load rotation key %q: %w", kid, err)
			}
			pubByKid[kid] = p
		}
		return &Issuer{
			alg:         AlgRS256,
			ttl:         cfg.TTL,
			issuer:      cfg.IssuerName,
			kid:         curKid,
			rsaPriv:     priv,
			rsaPub:      pub,
			rsaPubByKid: pubByKid,
		}, nil
	default:
		return nil, fmt.Errorf("authpkg: unsupported algorithm %q", cfg.Algorithm)
	}
}

// Algorithm 返回当前 Issuer 用的算法（main.go startup-safety 校验用）
func (i *Issuer) Algorithm() Algorithm { return i.alg }

// Issue 给 user 签一个 JWT。emailVerified / scopes 写到 claims。
func (i *Issuer) Issue(userID string, emailVerified bool, scopes ...string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(i.ttl)
	c := Claims{
		UserID: userID, EmailVerified: emailVerified, Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			Subject:   userID,
		},
	}
	var (
		tok    *jwt.Token
		signed string
		err    error
	)
	switch i.alg {
	case AlgHS256:
		tok = jwt.NewWithClaims(jwt.SigningMethodHS256, c)
		tok.Header["kid"] = i.kid
		signed, err = tok.SignedString(i.hsSecret)
	case AlgRS256:
		tok = jwt.NewWithClaims(jwt.SigningMethodRS256, c)
		tok.Header["kid"] = i.kid
		signed, err = tok.SignedString(i.rsaPriv)
	default:
		return "", time.Time{}, fmt.Errorf("authpkg: unsupported alg %q", i.alg)
	}
	return signed, exp, err
}

// Verify 解析 + 校验 JWT；返回 claims。
//
// 严格：
//   - token.alg 必须 == 本 Issuer 配置的 alg。拒 alg=none / HS256↔RS256 切换。
//   - RS256 路径走 kid 查找：token.header.kid 必须命中 rsaPubByKid（含
//     rotation extras）。kid 缺失时退回 active key 兼容（老 token 没 kid）。
func (i *Issuer) Verify(s string) (*Claims, error) {
	c := &Claims{}
	expectedAlg := string(i.alg)
	_, err := jwt.ParseWithClaims(s, c, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != expectedAlg {
			return nil, fmt.Errorf("unexpected signing method: got %s want %s", t.Method.Alg(), expectedAlg)
		}
		switch i.alg {
		case AlgHS256:
			return i.hsSecret, nil
		case AlgRS256:
			// kid header 用于 rotation：找对应 pub key；找不到才回退 active。
			// 找不到 kid → 直接报错而不是回退（防 attacker 假装 unknown kid 用旧 key）。
			if kidIface, ok := t.Header["kid"]; ok {
				if kid, ok := kidIface.(string); ok && kid != "" {
					if pub, ok := i.rsaPubByKid[kid]; ok {
						return pub, nil
					}
					return nil, fmt.Errorf("unknown kid %q (rotation registry mismatch)", kid)
				}
			}
			// 完全没 kid header：退回 active key（迁移期老 token 兼容）
			return i.rsaPub, nil
		}
		return nil, fmt.Errorf("authpkg: no key for alg %s", i.alg)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// loadRSAPrivate PEM file → *rsa.PrivateKey
func loadRSAPrivate(path string) (*rsa.PrivateKey, error) {
	if path == "" {
		return nil, errors.New("RS256 private key path empty")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM (private key)")
	}
	// 兼容 PKCS1 和 PKCS8 两种格式
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := k.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("PKCS8 key is not RSA")
	}
	return nil, errors.New("private key parse failed (tried PKCS1 + PKCS8)")
}

// loadRSAPublic PEM file → *rsa.PublicKey
func loadRSAPublic(path string) (*rsa.PublicKey, error) {
	if path == "" {
		return nil, errors.New("RS256 public key path empty")
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM (public key)")
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := k.(*rsa.PublicKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("PKIX key is not RSA")
	}
	if k, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("public key parse failed (tried PKIX + PKCS1)")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// HashPassword bcrypt cost 12 (~250ms@2024 hw)；登录路径只 verify 一次，不影响主路径。
func HashPassword(plain string) (string, error) {
	if len(plain) < 8 {
		return "", errors.New("password too short (min 8)")
	}
	if len(plain) > 72 {
		return "", errors.New("password too long (bcrypt 72 byte limit)")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), 12)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// CheckPassword 验证；常量时间，bcrypt 内部已实现。
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NewTOTPSecret 生成 TOTP base32 种子 + otpauth URL。
func NewTOTPSecret(issuer, accountName string) (secret, otpauthURL string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: accountName,
	})
	if err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

// VerifyTOTP base32 secret + 6 位 code。容差 +/- 1 step（30s）。
func VerifyTOTP(secret, code string) bool {
	if secret == "" || code == "" {
		return false
	}
	return totp.Validate(code, secret)
}

// GenerateNumericCode 6 位邮件 / 短信验证码（crypto-rand）。
func GenerateNumericCode(digits int) (string, error) {
	if digits <= 0 || digits > 10 {
		digits = 6
	}
	max := int64(1)
	for i := 0; i < digits; i++ {
		max *= 10
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := int64(b[0])<<56 | int64(b[1])<<48 | int64(b[2])<<40 | int64(b[3])<<32 |
		int64(b[4])<<24 | int64(b[5])<<16 | int64(b[6])<<8 | int64(b[7])
	if n < 0 {
		n = -n
	}
	v := n % max
	return fmt.Sprintf("%0*d", digits, v), nil
}

// RandomBase32Token 生成 32 字节随机 → 52 字符 base32。给 OTP challenge / API key 等用。
func RandomBase32Token() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]), nil
}

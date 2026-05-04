// Package authpkg JWT issue / verify + bcrypt + TOTP helpers。
//
// 设计：
//   - JWT 用 HS256 (secret-shared)。生产应换 RS256 + KMS-backed key
//   - SecretKey 来自 viper config: auth.jwt_secret（dev 默认值）
//   - Token TTL 默认 24h；refresh-token 路径暂不实现
package authpkg

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// Claims 写到 JWT 的 claims。
type Claims struct {
	UserID        string   `json:"sub"`
	EmailVerified bool     `json:"email_verified"`
	Scopes        []string `json:"scopes,omitempty"`
	jwt.RegisteredClaims
}

// Issuer 配 secret + 默认 ttl 后给所有用户签 token。
type Issuer struct {
	Secret []byte
	TTL    time.Duration
	Issuer string // "user-merchant-core"
}

func NewIssuer(secret string, ttl time.Duration) *Issuer {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Issuer{Secret: []byte(secret), TTL: ttl, Issuer: "user-merchant-core"}
}

// Issue 给 user 签一个 JWT。emailVerified / scopes 写到 claims。
func (i *Issuer) Issue(userID string, emailVerified bool, scopes ...string) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(i.TTL)
	c := Claims{
		UserID: userID, EmailVerified: emailVerified, Scopes: scopes,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			Subject:   userID,
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	s, err := tok.SignedString(i.Secret)
	return s, exp, err
}

// Verify 解析 + 校验 JWT；返回 claims。token 过期 / 签名错都返 error。
func (i *Issuer) Verify(s string) (*Claims, error) {
	c := &Claims{}
	_, err := jwt.ParseWithClaims(s, c, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != "HS256" {
			return nil, fmt.Errorf("unexpected signing method: %s", t.Method.Alg())
		}
		return i.Secret, nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
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

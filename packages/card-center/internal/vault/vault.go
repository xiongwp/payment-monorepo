// Package vault 实现 card-center 的纯密码学 token：
//
//	token := base64url(KMS.Encrypt(plaintext, AAD=binding))
//
// 没有数据库；token 本身就是 KMS-encrypted JSON 负载。decrypt 后校验 AAD +
// 时间戳即可。
package vault

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// StoredTokenPrefix 长期存卡 token
	StoredTokenPrefix = "tok_card_"
	// PaymentTokenPrefix 一次性支付 token
	PaymentTokenPrefix = "tok_pay_"

	// MaxPaymentTTL 一次性 token 上限 30 分钟（业务硬约束）
	MaxPaymentTTL = 30 * time.Minute
	// DefaultPaymentTTL 默认 30 分钟
	DefaultPaymentTTL = 30 * time.Minute
)

var (
	// ErrInvalidToken token 格式错或解码失败
	ErrInvalidToken = errors.New("vault: invalid token")
	// ErrTokenExpired payment token 已过期
	ErrTokenExpired = errors.New("vault: token expired")
	// ErrAADMismatch AAD 校验失败（token 被错绑用户 / 错绑 PI）
	ErrAADMismatch = errors.New("vault: AAD mismatch (token misbound)")
	// ErrKMSError KMS 调用失败
	ErrKMSError = errors.New("vault: kms call failed")
)

// KMS 抽象 kms-manage 客户端的最小接口（Encrypt/Decrypt with AAD）。
//
// 实现见 internal/kmsclient/client.go。生产路径走 mTLS 到 card DC 内部独立
// 部署的 kms-manage。
type KMS interface {
	Encrypt(ctx context.Context, plaintext []byte, aad string) (ciphertext string, kid string, err error)
	Decrypt(ctx context.Context, ciphertext string, aad string) (plaintext []byte, kid string, err error)
}

// Vault token vault 主类。无状态，无数据库；只持 KMS client。
type Vault struct {
	kms KMS
	now func() time.Time
}

// NewVault 构造。生产用 NewVault(kms, time.Now)。
func NewVault(kms KMS, now func() time.Time) *Vault {
	if now == nil {
		now = time.Now
	}
	return &Vault{kms: kms, now: now}
}

// ─── 存卡 token (long-lived) ────────────────────────────────────────────────

// storedPayload 存卡 token 内嵌的明文 JSON
type storedPayload struct {
	PAN        string `json:"pan"`
	ExpMonth   int    `json:"exp_month"`
	ExpYear    int    `json:"exp_year"`
	HolderName string `json:"holder_name,omitempty"`
	Nonce      string `json:"nonce"` // 同 PAN 多次 Tokenize 出不同 token
	IssuedAt   int64  `json:"iat"`   // unix 秒
}

// Tokenize 长期 token：AAD 锁 user_id。
func (v *Vault) Tokenize(ctx context.Context, userID, pan string, expMonth, expYear int, holderName string) (token, kid string, err error) {
	if userID == "" || pan == "" {
		return "", "", fmt.Errorf("%w: user_id and pan required", ErrInvalidToken)
	}
	nonce, err := randHex(16)
	if err != nil {
		return "", "", fmt.Errorf("%w: nonce: %v", ErrKMSError, err)
	}
	payload := storedPayload{
		PAN:        pan,
		ExpMonth:   expMonth,
		ExpYear:    expYear,
		HolderName: holderName,
		Nonce:      nonce,
		IssuedAt:   v.now().Unix(),
	}
	blob, err := json.Marshal(&payload)
	if err != nil {
		return "", "", fmt.Errorf("%w: marshal: %v", ErrInvalidToken, err)
	}
	aad := storedAAD(userID)
	ct, kid, err := v.kms.Encrypt(ctx, blob, aad)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrKMSError, err)
	}
	return StoredTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(ct)), kid, nil
}

// PeekStored 解出存卡 token（不外发 PAN，仅给内部 CreatePaymentToken 用）。
// 返回 plaintext payload。AAD 必须匹配 user_id。
func (v *Vault) peekStored(ctx context.Context, userID, storedToken string) (*storedPayload, error) {
	if !hasPrefix(storedToken, StoredTokenPrefix) {
		return nil, ErrInvalidToken
	}
	body := storedToken[len(StoredTokenPrefix):]
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalidToken
	}
	plain, _, err := v.kms.Decrypt(ctx, string(raw), storedAAD(userID))
	if err != nil {
		return nil, ErrAADMismatch
	}
	var p storedPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, ErrInvalidToken
	}
	return &p, nil
}

// ─── 一次性支付 token (30min TTL) ───────────────────────────────────────────

// payPayload 支付 token 内嵌的明文
type payPayload struct {
	PAN        string `json:"pan"`
	ExpMonth   int    `json:"exp_month"`
	ExpYear    int    `json:"exp_year"`
	HolderName string `json:"holder_name,omitempty"`
	PIID       string `json:"pi_id"`
	Amount     int64  `json:"amount"`
	Currency   string `json:"currency"`
	ExpiresAt  int64  `json:"exp"` // unix 秒
	Nonce      string `json:"nonce"`
	IssuedAt   int64  `json:"iat"`
}

// CreatePaymentToken 用 stored_token 派生一次性 payment token。
//
// 校验：
//  1. stored_token 解码 + user_id AAD 校验
//  2. ttl 不超过 MaxPaymentTTL
//
// 派生 payload AAD = "pay:pi:<pi_id>"。
func (v *Vault) CreatePaymentToken(ctx context.Context, userID, storedToken, piID string, amount int64, currency string, ttl time.Duration) (token, kid string, expiresAt time.Time, err error) {
	stored, err := v.peekStored(ctx, userID, storedToken)
	if err != nil {
		return "", "", time.Time{}, err
	}
	if piID == "" {
		return "", "", time.Time{}, fmt.Errorf("%w: pi_id required", ErrInvalidToken)
	}
	if ttl <= 0 || ttl > MaxPaymentTTL {
		ttl = DefaultPaymentTTL
	}
	now := v.now()
	exp := now.Add(ttl)
	nonce, err := randHex(16)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: nonce: %v", ErrKMSError, err)
	}
	payload := payPayload{
		PAN:        stored.PAN,
		ExpMonth:   stored.ExpMonth,
		ExpYear:    stored.ExpYear,
		HolderName: stored.HolderName,
		PIID:       piID,
		Amount:     amount,
		Currency:   currency,
		ExpiresAt:  exp.Unix(),
		Nonce:      nonce,
		IssuedAt:   now.Unix(),
	}
	blob, err := json.Marshal(&payload)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: marshal: %v", ErrInvalidToken, err)
	}
	ct, kid, err := v.kms.Encrypt(ctx, blob, payAAD(piID))
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: %v", ErrKMSError, err)
	}
	return PaymentTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(ct)), kid, exp, nil
}

// Detokenize 用 payment_token 换 PAN。仅 card-payment 服务调。
//
// 校验：
//  1. token 前缀 + base64 + KMS Decrypt
//  2. AAD 必须匹配 pi_id（防 token 转用其他 PI）
//  3. exp_ts 没过期
//
// 返回的 PAN 仅在 RPC 响应内出现，调用方接到后必须立刻用、立刻清栈。
type Detokenized struct {
	PAN        string
	ExpMonth   int
	ExpYear    int
	HolderName string
	PIID       string
	Amount     int64
	Currency   string
}

func (v *Vault) Detokenize(ctx context.Context, paymentToken, piID string) (*Detokenized, error) {
	if !hasPrefix(paymentToken, PaymentTokenPrefix) {
		return nil, ErrInvalidToken
	}
	body := paymentToken[len(PaymentTokenPrefix):]
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrInvalidToken
	}
	plain, _, err := v.kms.Decrypt(ctx, string(raw), payAAD(piID))
	if err != nil {
		return nil, ErrAADMismatch
	}
	var p payPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, ErrInvalidToken
	}
	// 双保险校验 pi_id（payload 内嵌 + AAD 已经验过；payload 字段被改也会让
	// AAD 失败，但显式 check 让逻辑更清晰）
	if p.PIID != piID {
		return nil, ErrAADMismatch
	}
	if v.now().Unix() > p.ExpiresAt {
		return nil, ErrTokenExpired
	}
	return &Detokenized{
		PAN:        p.PAN,
		ExpMonth:   p.ExpMonth,
		ExpYear:    p.ExpYear,
		HolderName: p.HolderName,
		PIID:       p.PIID,
		Amount:     p.Amount,
		Currency:   p.Currency,
	}, nil
}

// ─── helpers ────────────────────────────────────────────────────────────────

// storedAAD AAD for stored token: 锁 user_id，token 不能转给别的 user
func storedAAD(userID string) string { return "card:user_id:" + userID }

// payAAD AAD for payment token: 锁 pi_id，token 不能用到其他 PI
func payAAD(piID string) string { return "pay:pi:" + piID }

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0xf]
	}
	return string(out), nil
}

func hasPrefix(s, p string) bool {
	if len(p) > len(s) {
		return false
	}
	return s[:len(p)] == p
}

// MaskPAN BIN+last4，前端展示用。
func MaskPAN(pan string) string {
	if len(pan) < 12 {
		return pan
	}
	masked := make([]byte, len(pan))
	copy(masked, pan[:6])
	for i := 6; i < len(pan)-4; i++ {
		masked[i] = '*'
	}
	copy(masked[len(pan)-4:], pan[len(pan)-4:])
	return string(masked)
}

// DetectNetwork 根据 BIN 判断卡组织（简化版，生产应该用完整 BIN 表）。
func DetectNetwork(pan string) string {
	if len(pan) < 6 {
		return "unknown"
	}
	switch {
	case pan[0] == '4':
		return "visa"
	case pan[0] == '5' && pan[1] >= '1' && pan[1] <= '5':
		return "mastercard"
	case pan[:2] == "34" || pan[:2] == "37":
		return "amex"
	case pan[:2] == "35":
		return "jcb"
	case pan[:2] == "62":
		return "unionpay"
	}
	return "unknown"
}

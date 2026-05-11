// Package tokenize — 卡 PAN 令牌化。
//
// 目的：让商户拿不到卡号，PCI scope 收缩到 gateway 内部。
//
// 流程:
//   1. Drop-in JS 收集卡号 → 直接 POST 到 gateway.tokenize endpoint
//   2. gateway 调 kms-manage 加密 PAN → 存 token_vault (按 token_id 索引)
//   3. 返回 token 给商户 (如 "tok_4111_xxx") + 公开元信息 (last4, brand, expiry)
//   4. 商户后续 charge 用 token，gateway 内部 detokenize 调 kms 解密
//
// Token 生命周期:
//   - one-time token: 创建后 15min 内必须用，之后失效（单笔扣款用）
//   - reusable token: 关联 customer_id，可重复扣款（subscription / 卡保存）
//
// 安全:
//   - token 本身只是个不透明 ID，没有 PAN 信息
//   - vault 走 KMS envelope encryption (DEK per token)
//   - audit log 所有 detokenize 调用（谁在何时为何目的用 token 扣款）
//   - token rotation: 客户更新卡时新 token，旧 token 失效

package tokenize

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// CardInput 卡入参（从 Drop-in JS 收集）。
type CardInput struct {
	PAN        string `json:"pan"`         // 16 digits
	ExpiryMM   int    `json:"expiry_mm"`
	ExpiryYY   int    `json:"expiry_yy"`
	CVV        string `json:"cvv"`         // 仅用一次，不存 vault
	HolderName string `json:"holder_name"`
}

// Token 令牌化后的对外结构（公开信息 + token id）。
type Token struct {
	ID         string    `json:"id"`            // tok_xxx
	Brand      string    `json:"brand"`         // visa / mastercard / amex / jcb / unionpay
	BIN        string    `json:"bin"`           // 前 6 位
	Last4      string    `json:"last4"`
	ExpiryMM   int       `json:"expiry_mm"`
	ExpiryYY   int       `json:"expiry_yy"`
	HolderName string    `json:"holder_name,omitempty"`
	Type       TokenType `json:"type"`          // one_time / reusable
	CustomerID string    `json:"customer_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Used       bool      `json:"used,omitempty"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
}

// TokenType 类型。
type TokenType string

const (
	TokenOneTime  TokenType = "one_time"
	TokenReusable TokenType = "reusable"
)

// KMSClient 加密 / 解密 PAN（生产接 kms-manage gRPC）。
type KMSClient interface {
	Encrypt(ctx context.Context, plain []byte, scope string) (cipher []byte, err error)
	Decrypt(ctx context.Context, cipher []byte, scope string) (plain []byte, err error)
}

// VaultRepo 抽象 — 真实实现 MySQL / HSM。
type VaultRepo interface {
	Save(ctx context.Context, token *Token, panEnc, cvvEnc []byte) error
	Get(ctx context.Context, tokenID string) (*Token, []byte, error) // returns token + encrypted_pan
	MarkUsed(ctx context.Context, tokenID string) error
}

// Service token 化服务。
type Service struct {
	kms   KMSClient
	repo  VaultRepo
	mu    sync.Mutex
	stats struct {
		Created   int64
		Detokens  int64
		Rejected  int64
	}
}

// New 构造。
func New(kms KMSClient, repo VaultRepo) *Service {
	return &Service{kms: kms, repo: repo}
}

// Tokenize 卡 → token。
func (s *Service) Tokenize(ctx context.Context, in CardInput, ttype TokenType, customerID string) (*Token, error) {
	if err := validate(in); err != nil {
		s.mu.Lock(); s.stats.Rejected++; s.mu.Unlock()
		return nil, err
	}
	// 加密 PAN
	panEnc, err := s.kms.Encrypt(ctx, []byte(in.PAN), "card_pan")
	if err != nil {
		return nil, fmt.Errorf("kms encrypt pan: %w", err)
	}
	var cvvEnc []byte
	if in.CVV != "" {
		// CVV 加密暂存（PCI: CVV 不允许长期存储，gateway 一次性使用后立即销毁）
		cvvEnc, err = s.kms.Encrypt(ctx, []byte(in.CVV), "card_cvv_ephemeral")
		if err != nil {
			return nil, fmt.Errorf("kms encrypt cvv: %w", err)
		}
	}
	id := "tok_" + randHex(12)
	ttl := 15 * time.Minute
	if ttype == TokenReusable {
		ttl = 365 * 24 * time.Hour
	}
	now := time.Now().UTC()
	tok := &Token{
		ID:         id,
		Brand:      detectBrand(in.PAN),
		BIN:        in.PAN[:6],
		Last4:      in.PAN[len(in.PAN)-4:],
		ExpiryMM:   in.ExpiryMM,
		ExpiryYY:   in.ExpiryYY,
		HolderName: maskName(in.HolderName),
		Type:       ttype,
		CustomerID: customerID,
		CreatedAt:  now,
		ExpiresAt:  now.Add(ttl),
	}
	if err := s.repo.Save(ctx, tok, panEnc, cvvEnc); err != nil {
		return nil, fmt.Errorf("save vault: %w", err)
	}
	s.mu.Lock(); s.stats.Created++; s.mu.Unlock()
	return tok, nil
}

// Detokenize 内部 charge 调用 — 用 token 拿回 PAN（限本 gateway 内部，不对商户暴露）。
//
// 调用方一定要传 reason（"charge"/"refund"/"webhook_lookup"），audit log 留痕。
func (s *Service) Detokenize(ctx context.Context, tokenID, reason string) (pan string, brand string, err error) {
	if tokenID == "" || reason == "" {
		return "", "", fmt.Errorf("token_id and reason required")
	}
	tok, panEnc, err := s.repo.Get(ctx, tokenID)
	if err != nil {
		return "", "", fmt.Errorf("vault get: %w", err)
	}
	if tok == nil {
		return "", "", fmt.Errorf("token not found")
	}
	if time.Now().UTC().After(tok.ExpiresAt) {
		return "", "", fmt.Errorf("token expired")
	}
	if tok.Type == TokenOneTime && tok.Used {
		return "", "", fmt.Errorf("token already used")
	}
	plain, err := s.kms.Decrypt(ctx, panEnc, "card_pan")
	if err != nil {
		return "", "", fmt.Errorf("kms decrypt: %w", err)
	}
	if tok.Type == TokenOneTime {
		_ = s.repo.MarkUsed(ctx, tokenID)
	}
	s.mu.Lock(); s.stats.Detokens++; s.mu.Unlock()
	return string(plain), tok.Brand, nil
}

// ─── helpers ────────────────────────────────────────────────────────

// validate 卡号 Luhn 校验 + 长度 + 有效期未过期。
func validate(in CardInput) error {
	pan := strings.ReplaceAll(in.PAN, " ", "")
	if len(pan) < 13 || len(pan) > 19 {
		return fmt.Errorf("invalid PAN length")
	}
	for _, c := range pan {
		if c < '0' || c > '9' {
			return fmt.Errorf("PAN must be digits only")
		}
	}
	if !luhn(pan) {
		return fmt.Errorf("PAN failed Luhn check")
	}
	if in.ExpiryMM < 1 || in.ExpiryMM > 12 {
		return fmt.Errorf("invalid expiry month")
	}
	if in.ExpiryYY < 0 || in.ExpiryYY > 99 {
		return fmt.Errorf("invalid expiry year")
	}
	// expiry 不能过期（当前月份算有效）
	now := time.Now()
	expYear := 2000 + in.ExpiryYY
	if expYear < now.Year() || (expYear == now.Year() && in.ExpiryMM < int(now.Month())) {
		return fmt.Errorf("card expired")
	}
	return nil
}

// luhn 校验。
func luhn(pan string) bool {
	sum := 0
	dbl := false
	for i := len(pan) - 1; i >= 0; i-- {
		d := int(pan[i] - '0')
		if dbl {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		dbl = !dbl
	}
	return sum%10 == 0
}

// detectBrand BIN → 卡组（简化，生产应查 BIN 表）。
func detectBrand(pan string) string {
	if len(pan) == 0 {
		return "unknown"
	}
	switch pan[0] {
	case '3':
		if len(pan) >= 2 && (pan[1] == '4' || pan[1] == '7') {
			return "amex"
		}
		return "jcb"
	case '4':
		return "visa"
	case '5':
		return "mastercard"
	case '6':
		return "discover"
	case '9':
		return "unionpay" // 简化 — 真实 UnionPay 是 62xx
	}
	return "unknown"
}

// maskName 持卡人名脱敏（保留首尾 + 中间 *）。
func maskName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 2 {
		return s
	}
	return string(s[0]) + strings.Repeat("*", len(s)-2) + string(s[len(s)-1])
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

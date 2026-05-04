package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
)

// ─── 3D Secure Provider ───────────────────────────────────────────────────────

// ThreeDSProvider 3DS 挑战的 ActionProvider 实现。
// 注入 channel.ThreeDSClient 即可，由下游 acquirer adapter（Adyen / Stripe / 自研）实现接口。
type ThreeDSProvider struct {
	Client channel.ThreeDSClient
}

// NewThreeDSProvider 构造
func NewThreeDSProvider(c channel.ThreeDSClient) *ThreeDSProvider { return &ThreeDSProvider{Client: c} }

// Initiate 走渠道下发 3DS 挑战
func (p *ThreeDSProvider) Initiate(ctx context.Context, in *InitiateActionInput) (*ActionChallenge, error) {
	if p.Client == nil {
		return nil, fmt.Errorf("3ds client not configured")
	}
	chargeID := ""
	if in.Charge != nil {
		chargeID = in.Charge.ID
	}
	req := channel.ThreeDSChallengeRequest{
		PaymentIntentID:  in.PaymentIntent.ID,
		ChargeID:         chargeID,
		Amount:           in.PaymentIntent.Amount,
		Currency:         in.PaymentIntent.Currency,
		PaymentMethodRef: in.PaymentIntent.PaymentMethod,
		ReturnURL:        in.PaymentIntent.ReturnURL,
		Extra:            in.Extra,
	}
	ch, err := p.Client.CreateRedirect(ctx, req)
	if err != nil {
		return nil, err
	}
	var expAt *time.Time
	if !ch.ExpiresAt.IsZero() {
		t := ch.ExpiresAt
		expAt = &t
	}
	return &ActionChallenge{
		Payload: map[string]string{
			"redirect_url": ch.RedirectURL,
			"session_id":   ch.SessionID,
			"return_url":   in.PaymentIntent.ReturnURL,
		},
		ExpectedSecret: ch.SessionID,
		ExpiresAt:      expAt,
		MaxAttempts:    1,
	}, nil
}

// Verify ACS 回调字段校验
func (p *ThreeDSProvider) Verify(ctx context.Context, action *domain.PayAction, input map[string]string) (bool, string, error) {
	if p.Client == nil {
		return false, "", fmt.Errorf("3ds client not configured")
	}
	return p.Client.VerifyCallback(ctx, action.ExpectedSecret, input)
}

// ─── OTP Provider ─────────────────────────────────────────────────────────────

// OTPProvider OTP 挑战的 ActionProvider 实现。
//
//	Sender     必填：短信 / 邮件 / 语音 / 银行 OTP 等实现 channel.OTPSender 接口
//	Verifier   可选：渠道不返回明文 Code 时用它反查；否则服务端直接本地比对
//	TTL        过期时间；零值时服务层用默认值
//	MaxAttempt 默认允许尝试次数
type OTPProvider struct {
	Sender     channel.OTPSender
	Verifier   channel.OTPVerifier
	Purpose    channel.OTPPurpose
	TTL        time.Duration
	MaxAttempt int
}

// NewOTPProvider 构造
func NewOTPProvider(sender channel.OTPSender, purpose channel.OTPPurpose) *OTPProvider {
	if purpose == "" {
		purpose = channel.OTPPurposePayment
	}
	return &OTPProvider{Sender: sender, Purpose: purpose, TTL: 5 * time.Minute, MaxAttempt: 3}
}

// Initiate 调下游下发 OTP
func (p *OTPProvider) Initiate(ctx context.Context, in *InitiateActionInput) (*ActionChallenge, error) {
	if p.Sender == nil {
		return nil, fmt.Errorf("otp sender not configured")
	}
	res, err := p.Sender.Send(ctx, channel.OTPSendRequest{
		PaymentIntentID: in.PaymentIntent.ID,
		CustomerID:      in.PaymentIntent.CustomerID,
		Amount:          in.PaymentIntent.Amount,
		Currency:        in.PaymentIntent.Currency,
		Purpose:         p.Purpose,
		Extra:           in.Extra,
	})
	if err != nil {
		return nil, err
	}

	var expAt *time.Time
	if !res.ExpiresAt.IsZero() {
		t := res.ExpiresAt
		expAt = &t
	} else if p.TTL > 0 {
		t := time.Now().UTC().Add(p.TTL)
		expAt = &t
	}
	return &ActionChallenge{
		Payload: map[string]string{
			"recipient_masked": res.RecipientMasked,
			"channel":          res.Channel,
		},
		ExpectedSecret: res.Code, // 渠道不返回明文时此处为 challenge_id
		ExpiresAt:      expAt,
		MaxAttempts:    p.MaxAttempt,
	}, nil
}

// Verify 用户提交 "otp" 字段；优先走 Verifier，否则本地比对
func (p *OTPProvider) Verify(ctx context.Context, action *domain.PayAction, input map[string]string) (bool, string, error) {
	got := strings.TrimSpace(input["otp"])
	if got == "" {
		return false, "otp empty", nil
	}
	if p.Verifier != nil {
		return p.Verifier.Verify(ctx, action.ExpectedSecret, got)
	}
	if got != action.ExpectedSecret {
		return false, "otp mismatch", nil
	}
	return true, "", nil
}

// ─── Pay Password Provider ────────────────────────────────────────────────────

// PayPasswordProvider 支付密码 ActionProvider 实现。
// 通过 channel.PasswordHashStore 取用户存量哈希，用户提交明文后本地算 SHA256 比对。
type PayPasswordProvider struct {
	Store channel.PasswordHashStore
}

// NewPayPasswordProvider 构造
func NewPayPasswordProvider(s channel.PasswordHashStore) *PayPasswordProvider {
	return &PayPasswordProvider{Store: s}
}

// Initiate 取哈希放入 ExpectedSecret；payload 只告诉前端要用哪种算法
func (p *PayPasswordProvider) Initiate(ctx context.Context, in *InitiateActionInput) (*ActionChallenge, error) {
	if p.Store == nil {
		return nil, fmt.Errorf("password store not configured")
	}
	algo, hash, err := p.Store.GetPasswordHash(ctx, in.PaymentIntent.CustomerID)
	if err != nil {
		return nil, err
	}
	if algo == "" {
		algo = "sha256"
	}
	return &ActionChallenge{
		Payload: map[string]string{
			"algorithm": algo,
		},
		ExpectedSecret: hash,
		MaxAttempts:    3,
	}, nil
}

// Verify 用户提交 "password" 明文；计算 SHA256 哈希与 ExpectedSecret 比对（小写 hex）
func (p *PayPasswordProvider) Verify(_ context.Context, action *domain.PayAction, input map[string]string) (bool, string, error) {
	pw := strings.TrimSpace(input["password"])
	if pw == "" {
		return false, "password empty", nil
	}
	sum := sha256.Sum256([]byte(pw))
	got := hex.EncodeToString(sum[:])
	if got != strings.ToLower(action.ExpectedSecret) {
		return false, "password mismatch", nil
	}
	return true, "", nil
}

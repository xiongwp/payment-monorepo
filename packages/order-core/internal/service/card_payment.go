// Package service 加 PI Confirm 路径的卡支付分支。
//
// 整体流程（用户选了某张存卡支付）：
//
//	1. CreatePI: 业务层带 user_id + user_card_id 创建 PI，写入 user_card_id 字段
//	2. Confirm:  本服务调 ConfirmCardPayment：
//	   a) 从 user-merchant-core 拿 stored_token (mTLS gRPC 内部接口)
//	   b) 调 card-center.CreatePaymentToken 派生 30min 一次性 token
//	   c) 把 payment_token 作为 channel_token 字段塞进 ChargeRequest
//	   d) payment-channel.adapter[card] 收到 channel_token 后 mTLS 调 card-payment
//	   e) card-payment 调 card-center.Detokenize → PAN → Visa Net → 返响应
//
// 关键纪律：order-core 进程内**永远没 PAN**，只有 stored_token + payment_token。
package service

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/order-core/internal/cardcenterclient"
	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/domain"
	"github.com/xiongwp/order-core/internal/usermerchantclient"
)

// CardPaymentDeps 卡支付所需的下游依赖
type CardPaymentDeps struct {
	UserMerchant usermerchantclient.CardLookup
	CardCenter   *cardcenterclient.Client
	Logger       *zap.Logger
}

// CardPaymentService 编排 user-merchant-core + card-center 拿 payment_token
type CardPaymentService struct {
	deps CardPaymentDeps
}

// NewCardPaymentService 构造
func NewCardPaymentService(deps CardPaymentDeps) *CardPaymentService {
	return &CardPaymentService{deps: deps}
}

// PreparePaymentToken 给 PI Confirm 用：
//
//	(user_id, user_card_id) → stored_token → card-center.CreatePaymentToken → payment_token
//
// 返回的 payment_token 跟 pi_id 绑死，TTL 30min，Confirm 成功后立刻给 channel adapter。
func (s *CardPaymentService) PreparePaymentToken(ctx context.Context, pi *domain.PaymentIntent) (paymentToken, maskedPAN, network string, err error) {
	if pi == nil || pi.UserID == 0 || pi.UserCardID == 0 {
		return "", "", "", fmt.Errorf("PreparePaymentToken: pi.user_id and pi.user_card_id required for card payment")
	}
	if s.deps.UserMerchant == nil || s.deps.CardCenter == nil {
		return "", "", "", fmt.Errorf("PreparePaymentToken: dependencies not configured")
	}

	// 1) 从 user-merchant-core 拿 stored_token
	stored, mp, nw, err := s.deps.UserMerchant.GetStoredTokenForPayment(ctx, pi.UserID, pi.UserCardID)
	if err != nil {
		s.deps.Logger.Warn("user-merchant lookup failed",
			zap.Int64("user_id", pi.UserID),
			zap.Int64("user_card_id", pi.UserCardID),
			zap.Error(err))
		return "", "", "", fmt.Errorf("lookup stored token: %w", err)
	}
	if stored == "" {
		return "", "", "", fmt.Errorf("user_card has no stored_token (deleted/expired?)")
	}

	// 2) 调 card-center.CreatePaymentToken 派生支付 token
	resp, err := s.deps.CardCenter.CreatePaymentToken(ctx, &cardcenterclient.CreatePaymentTokenRequest{
		StoredToken: stored,
		UserID:      pi.UserID,
		PIID:        pi.ID,
		Amount:      pi.Amount,
		Currency:    pi.Currency,
		TTL:         30 * time.Minute,
		TraceID:     "", // 由调用方上层 ctx 透传
	})
	if err != nil {
		s.deps.Logger.Error("card-center CreatePaymentToken failed",
			zap.String("pi_id", pi.ID), zap.Error(err))
		return "", "", "", fmt.Errorf("create payment token: %w", err)
	}

	// 优先用 card-center 返的 masked / network；fallback 到 user-merchant 的字段
	if resp.MaskedPAN != "" {
		mp = resp.MaskedPAN
	}
	if resp.Network != "" {
		nw = resp.Network
	}

	return resp.PaymentToken, mp, nw, nil
}

// AttachChannelToken 把 payment_token 写到 PaymentRequest.PaymentMethodRef 字段。
// payment-channel.adapter[card] 在 Charge 入口读 PaymentMethodRef，调 card-payment 时
// 当作 channel_token / payment_token 透传。
//
// 历史：之前误写成 channel.ChargeRequest.ChannelToken，类型 / 字段名都不存在。
// 当前命名跟 channel.PaymentRequest 一致；下游 adapter 已有 PaymentMethodRef 解析。
func AttachChannelToken(req *channel.PaymentRequest, paymentToken string) {
	if req == nil {
		return
	}
	req.PaymentMethodRef = paymentToken
}

// Package card 是 payment-channel 的"卡支付"渠道 adapter — Kitex 调 card-payment.
//
// 它**不直接**调 Visa / Mastercard, 而是通过 Kitex 调隔离 DC 内的 card-payment
// 服务, 由 card-payment 在 SAQ-D 范围内拿 PAN 调卡组织.
//
// 跟现有 15 个 adapter (gcash / maya / ...) 同形: 实现 channel.Adapter 接口.
//
// payment-channel 自己**不见 PAN**: 传入的 ChargeRequest.Metadata["payment_token"]
// 应当是 card-center 颁发的 payment_token (跟 pi_id AAD-bound, TTL 30min), 本
// adapter 只透传给 card-payment.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (card-payment) 已同步切.
// mTLS 不需要 (内部 mesh 明文).
package card

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"go.uber.org/zap"

	cardpaymentv1 "reconcile-system/packages/card-payment/kitex_gen/cardpayment/v1"
	cardpaymentservice "reconcile-system/packages/card-payment/kitex_gen/cardpayment/v1/cardpaymentservice"

	"github.com/xiongwp/payment-channel/internal/channel"
)

// Config card adapter 配置
type Config struct {
	// CardPaymentEndpoint card-payment 服务的 Kitex 地址 (host:port)
	CardPaymentEndpoint string
	// RPC 超时
	RPCTimeout time.Duration
}

// Adapter 实现 channel.Adapter
type Adapter struct {
	api     cardpaymentservice.Client
	logger  *zap.Logger
	timeout time.Duration
	mocked  bool // dev / staging 路径无 card-payment 时 short-circuit
}

// New dial card-payment via Kitex.
func New(cfg Config, logger *zap.Logger) (*Adapter, error) {
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 30 * time.Second
	}
	if cfg.CardPaymentEndpoint == "" {
		// dev / staging 没接 card-payment 时 mock 模式
		logger.Warn("card adapter: card-payment endpoint not configured, running in mock mode")
		return &Adapter{logger: logger, timeout: cfg.RPCTimeout, mocked: true}, nil
	}
	api, err := cardpaymentservice.NewClient("card-payment",
		client.WithHostPorts(cfg.CardPaymentEndpoint),
		client.WithRPCTimeout(cfg.RPCTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("dial card-payment (kitex): %w", err)
	}
	return &Adapter{api: api, logger: logger, timeout: cfg.RPCTimeout}, nil
}

// Name implements channel.Adapter
func (a *Adapter) Name() string { return "card" }

// Charge 把 ChargeRequest 透传给 card-payment.Authorize.
//
// mock 模式直接返成功 (dev/staging); 否则调 Kitex.
func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	if req.PiID == "" || req.IdempotencyKey == "" {
		return nil, fmt.Errorf("card: pi_id / idempotency_key required")
	}
	paymentToken := req.Metadata["payment_token"]
	if paymentToken == "" {
		return nil, fmt.Errorf("card: metadata[payment_token] required")
	}
	if a.mocked {
		return &channel.ChargeResponse{
			Result:        channel.ResultSucceeded,
			ExternalRefNo: "vmock_" + req.IdempotencyKey,
		}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.api.Authorize(cctx, &cardpaymentv1.AuthorizeRequest{
		PaymentToken:       paymentToken,
		PiId:               req.PiID,
		Amount:             req.AmountMinor,
		Currency:           req.Currency,
		MerchantDescriptor: req.MerchantDescriptor,
	})
	if err != nil {
		return nil, fmt.Errorf("card Authorize rpc: %w", err)
	}
	return &channel.ChargeResponse{
		Result:        channel.Result(resp.GetResult()),
		ExternalRefNo: resp.GetExternalRefNo(),
	}, nil
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.api.Capture(cctx, &cardpaymentv1.CaptureRequest{
		ExternalRefNo: req.ExternalRefNo,
		Amount:        req.AmountMinor,
	})
	if err != nil {
		return nil, fmt.Errorf("card Capture rpc: %w", err)
	}
	return &channel.OpResponse{Result: channel.Result(resp.GetResult()), ExternalRefNo: resp.GetExternalRefNo()}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.api.Void(cctx, &cardpaymentv1.VoidRequest{ExternalRefNo: req.ExternalRefNo})
	if err != nil {
		return nil, fmt.Errorf("card Void rpc: %w", err)
	}
	return &channel.OpResponse{Result: channel.Result(resp.GetResult()), ExternalRefNo: resp.GetExternalRefNo()}, nil
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: "vrf_" + req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.api.Refund(cctx, &cardpaymentv1.RefundRequest{
		ExternalRefNo: req.ExternalRefNo,
		Amount:        req.AmountMinor,
	})
	if err != nil {
		return nil, fmt.Errorf("card Refund rpc: %w", err)
	}
	return &channel.OpResponse{Result: channel.Result(resp.GetResult()), ExternalRefNo: resp.GetExternalRefNo()}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	if a.mocked {
		return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.api.Query(cctx, &cardpaymentv1.QueryRequest{ExternalRefNo: req.ExternalRefNo})
	if err != nil {
		return nil, fmt.Errorf("card Query rpc: %w", err)
	}
	return &channel.QueryResponse{Result: channel.Result(resp.GetResult()), ExternalRefNo: resp.GetExternalRefNo()}, nil
}

// ParseWebhook 卡支付 webhook 来源是 card-payment (独立 DC 通过 Kitex 推回);
// 本 adapter 不直接接 Visa / Mastercard webhook. payment-channel 暴露
// /internal/card-payment/webhook 给 card-payment 服务 POST, 走另一条独立路径.
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	_ = headers
	_ = body
	return nil, errors.New("card adapter: webhook should come from card-payment via internal channel, not direct from network")
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (a *Adapter) Close() error { return nil }

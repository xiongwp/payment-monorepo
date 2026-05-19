// Package card 卡支付 channel adapter — Kitex 直连 card-payment.
//
// 上游调用契约: ChargeRequest.Metadata["payment_token"] 必须由 caller 预先填好
// (在 order-core PI Confirm 路径上, 是 card-center.CreatePaymentToken 兑出来的
// 一次性 token), 否则 Charge 拒收. 这一层只是 channel.Adapter 接口的实现, 不接触
// PAN, 不接触 stored_token.
package card

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"
	"go.uber.org/zap"

	cardpaymentv1 "github.com/xiongwp/card-payment/kitex_gen/cardpayment/v1"
	cardpaymentservice "github.com/xiongwp/card-payment/kitex_gen/cardpayment/v1/cardpayment"

	"github.com/xiongwp/payment-channel/internal/channel"
)

// Config card adapter 配置.
type Config struct {
	CardPaymentEndpoint string
	RPCTimeout          time.Duration
}

// Adapter 实现 channel.Adapter (Kitex client to card-payment).
type Adapter struct {
	cli     cardpaymentservice.Client
	logger  *zap.Logger
	timeout time.Duration
}

// New 构造真 Kitex adapter. Endpoint 为空时返错 (caller 决定是否降级).
func New(cfg Config, logger *zap.Logger) (*Adapter, error) {
	if cfg.CardPaymentEndpoint == "" {
		return nil, errors.New("card adapter: CardPaymentEndpoint required")
	}
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 30 * time.Second
	}
	cli, err := cardpaymentservice.NewClient("card-payment",
		client.WithHostPorts(cfg.CardPaymentEndpoint),
		client.WithTransportProtocol(transport.GRPC),		client.WithRPCTimeout(cfg.RPCTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("dial card-payment: %w", err)
	}
	if logger != nil {
		logger.Info("card adapter wired",
			zap.String("endpoint", cfg.CardPaymentEndpoint),
			zap.Duration("rpc_timeout", cfg.RPCTimeout))
	}
	return &Adapter{cli: cli, logger: logger, timeout: cfg.RPCTimeout}, nil
}

func (a *Adapter) Name() string { return "card" }

// Charge 调 card-payment.Authorize. 要求 req.Metadata["payment_token"] 非空
// (由 order-core PI Confirm 阶段 card-center.CreatePaymentToken 兑出, 通过
// payment-core 透传到这里).
func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	token := ""
	network := ""
	if req.Metadata != nil {
		token = req.Metadata["payment_token"]
		network = strings.ToLower(req.Metadata["network"])
	}
	if token == "" {
		return nil, errors.New("card adapter: missing payment_token in Metadata")
	}
	resp, err := a.cli.Authorize(ctx, &cardpaymentv1.AuthorizeRequest{
		PaymentToken:       token,
		PiId:               req.PiID,
		Amount:             req.Amount,
		Currency:           req.Currency,
		Network:            network,
		MerchantDescriptor: req.Description,
		TraceId:            req.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("card-payment Authorize: %w", err)
	}
	return &channel.ChargeResponse{
		Result:         statusToResult(resp.GetStatus()),
		ExternalRefNo:  resp.GetNetworkRefNo(),
		FailureCode:    resp.GetDeclineCode(),
		FailureMessage: resp.GetDeclineReason(),
	}, nil
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	resp, err := a.cli.Capture(ctx, &cardpaymentv1.CaptureRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
		Amount:       req.Amount,
		TraceId:      req.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("card-payment Capture: %w", err)
	}
	return &channel.OpResponse{
		Result:        statusToResult(resp.GetStatus()),
		ExternalRefNo: resp.GetNetworkRefNo(),
	}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	resp, err := a.cli.Void(ctx, &cardpaymentv1.VoidRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
		TraceId:      req.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("card-payment Void: %w", err)
	}
	return &channel.OpResponse{
		Result:        statusToResult(resp.GetStatus()),
		ExternalRefNo: resp.GetNetworkRefNo(),
	}, nil
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	resp, err := a.cli.Refund(ctx, &cardpaymentv1.RefundRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
		Amount:       req.Amount,
		Reason:       req.Reason,
		TraceId:      req.IdempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("card-payment Refund: %w", err)
	}
	return &channel.OpResponse{
		Result:        statusToResult(resp.GetStatus()),
		ExternalRefNo: resp.GetRefundRefNo(),
	}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	resp, err := a.cli.Query(ctx, &cardpaymentv1.QueryRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
	})
	if err != nil {
		return nil, fmt.Errorf("card-payment Query: %w", err)
	}
	return &channel.QueryResponse{
		Result:        statusToResult(resp.GetStatus()),
		ExternalRefNo: resp.GetNetworkRefNo(),
	}, nil
}

// ParseWebhook card 卡组织 webhook 解析逻辑迁到 card-payment 内部 (网络/签名校验),
// 本 adapter 不再直接解析. 上游 webhook 入口已经在 card-payment.WebhookHandler.
func (a *Adapter) ParseWebhook(_ map[string]string, _ []byte) (*channel.WebhookEvent, error) {
	return nil, errors.New("card adapter: webhook parsing handled in card-payment service")
}

func (a *Adapter) Close() error { return nil }

// statusToResult card-payment 返的 "approved"/"declined"/"pending" 映射 channel.ResultType.
func statusToResult(status string) channel.ResultType {
	switch strings.ToLower(status) {
	case "approved", "captured", "succeeded", "success":
		return channel.ResultTypeSucceeded
	case "pending", "processing":
		return channel.ResultTypeProcessing
	case "declined", "failed", "error":
		return channel.ResultTypeFailed
	}
	return channel.ResultTypeProcessing
}

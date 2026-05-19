// Package paymentcoreclient 提供一个实现了 channel.PaymentChannel 的 Kitex 客户端,
// 后端指向 payment-core 的 PaymentCoreService.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (payment-core) 已同步切.
// mTLS 不需要 (内部 mesh 明文). 老 retry policy + 8MB max msg size + keepalive
// 暂留 TODO, Kitex 默认值大多够用.
package paymentcoreclient

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"

	paymentcorev1 "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1"
	paymentcoreservice "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1/paymentcoreservice"

	"github.com/xiongwp/order-core/internal/channel"
)

// Client 实现 channel.PaymentChannel, 把每个方法翻成对 payment-core 的 Kitex 调用.
type Client struct {
	name string
	api  paymentcoreservice.Client
	rpcT time.Duration
}

// Dial 建立到 payment-core 的 Kitex 连接.
// name 是这个 channel 在 channel registry 里的注册名 (历史上都是 "payment-core").
func Dial(name string, registry []string, endpoint string, rpcTimeout time.Duration) (*Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 10 * time.Second
	}
	const serviceName = "payment-core"
	opts := []client.Option{
		client.WithRPCTimeout(rpcTimeout),
		client.WithHostPorts(endpoint),
		client.WithTransportProtocol(transport.GRPC),		// TODO: 接 etcd resolver — client.WithResolver(kitexutil.NewEtcdResolver(etcdCli, ""))
		// TODO: per-method retry policy — UNAVAILABLE 最多 3 次
		// TODO: shadow + trace MW (port 老 grpc interceptor)
	}
	_ = registry

	api, err := paymentcoreservice.NewClient(serviceName, opts...)
	if err != nil {
		return nil, fmt.Errorf("paymentcoreclient kitex dial: %w", err)
	}
	return &Client{name: name, api: api, rpcT: rpcTimeout}, nil
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (c *Client) Close() error { return nil }

// Name 返回注册名。
func (c *Client) Name() string { return c.name }

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.rpcT)
}

// Charge 发起扣款（或预授权 / 异步受理 / requires_action）。
func (c *Client) Charge(ctx context.Context, req channel.PaymentRequest) (*channel.PaymentResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	out, err := c.api.Charge(ctx, &paymentcorev1.ChargeRequest{
		PaymentIntentId:  req.PaymentIntentID,
		ChargeId:         req.ChargeID,
		Amount:           req.Amount,
		Currency:         req.Currency,
		Country:          req.Metadata["country"],
		PaymentMethod:    req.PaymentMethod,
		PaymentMethodRef: req.PaymentMethodRef,
		CustomerId:       req.CustomerID,
		CaptureMethod:    req.CaptureMethod,
		ReturnUrl:        req.ReturnURL,
		NotifyUrl:        req.NotifyURL,
		Description:      req.Description,
		Metadata:         req.Metadata,
		Extra:            req.Extra,
	})
	if err != nil {
		return nil, err
	}
	return fromProtoCharge(out), nil
}

func (c *Client) Capture(ctx context.Context, req channel.CaptureRequest) (*channel.CaptureResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	out, err := c.api.Capture(ctx, &paymentcorev1.CaptureRequest{
		PaymentIntentId: req.PaymentIntentID,
		ChargeId:        req.ChargeID,
		ExternalRefNo:   req.ExternalRefNo,
		Amount:          req.Amount,
		Extra:           req.Extra,
	})
	if err != nil {
		return nil, err
	}
	return &channel.CaptureResponse{
		ResultType:     channel.PaymentResultType(out.GetResultType()),
		ExternalRefNo:  out.GetExternalRefNo(),
		AmountCaptured: req.Amount,
		FailureCode:    out.GetFailureCode(),
		FailureMessage: out.GetFailureMessage(),
	}, nil
}

func (c *Client) Void(ctx context.Context, req channel.VoidRequest) (*channel.VoidResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	out, err := c.api.Void(ctx, &paymentcorev1.VoidRequest{
		PaymentIntentId: req.PaymentIntentID,
		ChargeId:        req.ChargeID,
		ExternalRefNo:   req.ExternalRefNo,
		Reason:          req.Reason,
		Extra:           req.Extra,
	})
	if err != nil {
		return nil, err
	}
	return &channel.VoidResponse{
		ResultType:     channel.PaymentResultType(out.GetResultType()),
		ExternalRefNo:  out.GetExternalRefNo(),
		FailureCode:    out.GetFailureCode(),
		FailureMessage: out.GetFailureMessage(),
	}, nil
}

func (c *Client) Refund(ctx context.Context, req channel.RefundChannelRequest) (*channel.RefundChannelResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	out, err := c.api.Refund(ctx, &paymentcorev1.RefundRequest{
		PaymentIntentId: req.PaymentIntentID,
		ChargeId:        req.ChargeID,
		RefundId:        req.RefundID,
		ExternalRefNo:   req.ExternalRefNo,
		Amount:          req.Amount,
		Currency:        req.Currency,
		Reason:          req.Reason,
		Extra:           req.Extra,
	})
	if err != nil {
		return nil, err
	}
	return &channel.RefundChannelResponse{
		ResultType:          channel.PaymentResultType(out.GetResultType()),
		ExternalRefundRefNo: out.GetExternalRefNo(),
		FailureCode:         out.GetFailureCode(),
		FailureMessage:      out.GetFailureMessage(),
	}, nil
}

func (c *Client) Query(ctx context.Context, req channel.QueryRequest) (*channel.QueryResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	out, err := c.api.Query(ctx, &paymentcorev1.QueryRequest{
		PaymentIntentId: req.PaymentIntentID,
		ChargeId:        req.ChargeID,
		ExternalRefNo:   req.ExternalRefNo,
	})
	if err != nil {
		return nil, err
	}
	return &channel.QueryResponse{
		ResultType:     channel.PaymentResultType(out.GetResultType()),
		ExternalRefNo:  out.GetExternalRefNo(),
		AmountCaptured: out.GetAmountCaptured(),
		AmountRefunded: out.GetAmountRefunded(),
		FailureCode:    out.GetFailureCode(),
		FailureMessage: out.GetFailureMessage(),
	}, nil
}

func (c *Client) ParseWebhook(ctx context.Context, headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	out, err := c.api.ParseWebhook(ctx, &paymentcorev1.ParseWebhookRequest{
		Adapter: headers["X-Adapter"],
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return nil, err
	}
	return &channel.WebhookEvent{
		EventID:         out.GetEventId(),
		EventType:       out.GetEventType(),
		PaymentIntentID: out.GetPaymentIntentId(),
		ChargeID:        out.GetChargeId(),
		RefundID:        out.GetRefundId(),
		ExternalRefNo:   out.GetExternalRefNo(),
		Amount:          out.GetAmount(),
		Timestamp:       time.Unix(out.GetTimestamp(), 0),
		RawPayload:      out.GetRawPayload(),
	}, nil
}

// fromProtoCharge 把 paymentcorev1.ChargeResponse 翻成 order-core 的 PaymentResponse。
func fromProtoCharge(r *paymentcorev1.ChargeResponse) *channel.PaymentResponse {
	out := &channel.PaymentResponse{
		ResultType:       channel.PaymentResultType(r.GetResultType()),
		ExternalRefNo:    r.GetExternalRefNo(),
		AuthCode:         r.GetAuthCode(),
		AmountAuthorized: r.GetAmountAuthorized(),
		AmountCaptured:   r.GetAmountCaptured(),
		FailureCode:      r.GetFailureCode(),
		FailureMessage:   r.GetFailureMessage(),
		RiskLevel:        r.GetRiskLevel(),
		RiskScore:        int(r.GetRiskScore()),
		RawResponse:      r.GetRawResponse(),
	}
	if ra := r.GetRequiredAction(); ra != nil {
		out.RequiredAction = &channel.RequiredAction{
			Type:         channel.RequiredActionType(ra.GetType()),
			Details:      ra.GetDetails(),
			ChallengeRef: ra.GetChallengeRef(),
			ExpiresAt:    time.Unix(ra.GetExpiresAt(), 0),
		}
	}
	return out
}

// 静态接口断言
var _ channel.PaymentChannel = (*Client)(nil)

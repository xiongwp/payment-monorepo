// Package paymentcoreclient 提供一个实现了 channel.PaymentChannel 的 gRPC 客户端，
// 后端指向 payment-core 的 PaymentCoreService。形状完全对齐 e2e/paymentcore_channel.go
// 里的 bufconn 测试版本，只是这一份是生产用：自己 dial 真实 endpoint + keepalive +
// 重试策略，跟 payment-core 自己对外的 channelclient 保持同款。
package paymentcoreclient

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	"github.com/xiongwp/payment-util/serviceregistry"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/order-core/internal/shadow"
	"github.com/xiongwp/order-core/internal/trace"
)

// Client 实现 channel.PaymentChannel，把每个方法翻成对 payment-core 的 gRPC 调用。
type Client struct {
	name string
	conn *grpc.ClientConn
	api  paymentcorev1.PaymentCoreServiceClient
	rpcT time.Duration
}

// Dial 建立到 payment-core 的连接并返回 Client。endpoint 形如 "payment-core:9090"。
// name 是这个 channel 在 channel registry 里的注册名（历史上都是 "payment-core"）。
//
// registry 非空时走 etcd resolver（联栈多 pod 部署必走）；为空时退回 endpoint 直连
// （单仓 dev / 单机 docker run）。两条路都用 round_robin LB。
func Dial(name string, registry []string, endpoint string, rpcTimeout time.Duration) (*Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 10 * time.Second
	}
	const serviceName = "payment-core" // etcd 里 payment-core 的注册名
	conn, err := serviceregistry.DialWithFallback(registry, serviceName, endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// 入站 x-trace-id + x-shadow 都需要自动透传到 outgoing metadata；
		// 用 ChainUnaryInterceptor 把两个 client interceptor 串起来。
		grpc.WithChainUnaryInterceptor(
			trace.UnaryClientInterceptor(),
			shadow.UnaryClientInterceptor(),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  500 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   10 * time.Second,
			},
			MinConnectTimeout: 2 * time.Second,
		}),
		grpc.WithDefaultServiceConfig(`{
  "methodConfig": [{
    "name": [{"service": "paymentcore.v1.PaymentCoreService"}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.2s",
      "maxBackoff": "2s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(8<<20),
			grpc.MaxCallSendMsgSize(8<<20),
		),
	)
	if err != nil {
		return nil, err
	}
	return &Client{
		name: name,
		conn: conn,
		api:  paymentcorev1.NewPaymentCoreServiceClient(conn),
		rpcT: rpcTimeout,
	}, nil
}

// Close 释放底层 gRPC 连接。
func (c *Client) Close() error { return c.conn.Close() }

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

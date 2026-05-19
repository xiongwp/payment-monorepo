//go:build e2e

// Package e2e 提供 order-core 端的 PaymentChannel 实现，后端走 payment-core
// 的 gRPC。这个文件只在 e2e 构建标签下编译，主构建 `go build ./...` 不依赖
// payment-core 模块。
package e2e

import (
	"context"
	"fmt"
	"strconv"
	"time"

	paymentcorev1 "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1"
	paymentcoreservice "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1/paymentcoreservice"

	"github.com/xiongwp/order-core/internal/channel"
	"github.com/xiongwp/payment-util/kitexutil"
)

// PaymentCoreGRPCChannel 实现 order-core 的 channel.PaymentChannel,
// 把调用翻译成 paymentcorev1 Kitex 请求发给 payment-core.
//
// 文件名仍叫 *_grpc.go 是历史包袱; 内部已切 Kitex.
type PaymentCoreGRPCChannel struct {
	name string
	cli  paymentcoreservice.Client
}

// NewPaymentCoreGRPCChannel 用 payment-core 端点构造 (空 = 走 kitexutil helper 兜底
// payment-core:9091, 或 PAYMENT_CORE_GRPC_ADDR env 覆盖).
func NewPaymentCoreGRPCChannel(name, endpoint string) *PaymentCoreGRPCChannel {
	opt := kitexutil.DefaultHostPorts("payment-core")
	if endpoint != "" {
		opt = client.WithHostPorts(endpoint)
	}
	return &PaymentCoreGRPCChannel{
		name: name,
		cli:  kitexutil.MustKitexClient(paymentcoreservice.NewClient("payment-core", opt)),
	}
}

func (c *PaymentCoreGRPCChannel) Name() string { return c.name }

func (c *PaymentCoreGRPCChannel) Charge(ctx context.Context, req channel.PaymentRequest) (*channel.PaymentResponse, error) {
	out, err := c.cli.Charge(ctx, &paymentcorev1.ChargeRequest{
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

func (c *PaymentCoreGRPCChannel) Capture(ctx context.Context, req channel.CaptureRequest) (*channel.CaptureResponse, error) {
	out, err := c.cli.Capture(ctx, &paymentcorev1.CaptureRequest{
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

func (c *PaymentCoreGRPCChannel) Void(ctx context.Context, req channel.VoidRequest) (*channel.VoidResponse, error) {
	out, err := c.cli.Void(ctx, &paymentcorev1.VoidRequest{
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

func (c *PaymentCoreGRPCChannel) Refund(ctx context.Context, req channel.RefundChannelRequest) (*channel.RefundChannelResponse, error) {
	out, err := c.cli.Refund(ctx, &paymentcorev1.RefundRequest{
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

func (c *PaymentCoreGRPCChannel) Query(ctx context.Context, req channel.QueryRequest) (*channel.QueryResponse, error) {
	out, err := c.cli.Query(ctx, &paymentcorev1.QueryRequest{
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

func (c *PaymentCoreGRPCChannel) ParseWebhook(ctx context.Context, headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	out, err := c.cli.ParseWebhook(ctx, &paymentcorev1.ParseWebhookRequest{
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

// fromProtoCharge 把 paymentcorev1.ChargeResponse 翻成 order-core 的
// channel.PaymentResponse，把 RequiredAction.Details 原样保留。
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

// _ helper 用来抑制 strconv 潜在的无使用告警（预留给将来增添字段类型转换）
var _ = strconv.Itoa
var _ = fmt.Sprintf

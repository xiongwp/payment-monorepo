// Package card 是 payment-channel 的"卡支付"渠道 adapter。
//
// 它**不直接**调 Visa / Mastercard,而是通过 mTLS gRPC 调隔离 DC 内的
// card-payment 服务,由 card-payment 在 SAQ-D 范围内拿 PAN 调卡组织。
//
// 跟现有 15 个 adapter (gcash / maya / ...) 同形:实现 channel.Adapter 接口。
//
// payment-channel 自己**不见 PAN**:传入的 ChargeRequest.Metadata["payment_token"]
// 应当是 card-center 颁发的 payment_token (跟 pi_id AAD-bound),本 adapter 只透传给
// card-payment。
package card

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	cardpaymentv1 "github.com/xiongwp/card-payment/api/proto/cardpayment/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/xiongwp/payment-channel/internal/channel"
)

// Config card adapter 配置
type Config struct {
	// CardPaymentEndpoint card-payment 服务的 mTLS gRPC 地址
	CardPaymentEndpoint string
	// mTLS 客户端证书(payment-channel 调 card-payment 用)
	ClientCert string
	ClientKey  string
	ServerCA   string
	// dev 用 insecure;prod 必须 false
	Insecure bool
	// RPC 超时
	RPCTimeout time.Duration
}

// Adapter 实现 channel.Adapter
type Adapter struct {
	conn    *grpc.ClientConn
	cli     cardpaymentv1.CardPaymentClient
	logger  *zap.Logger
	timeout time.Duration
	mocked  bool // dev / staging 路径无 card-payment 时 short-circuit
}

// New dial card-payment over mTLS(或 insecure for dev)
func New(cfg Config, logger *zap.Logger) (*Adapter, error) {
	if cfg.RPCTimeout == 0 {
		cfg.RPCTimeout = 30 * time.Second
	}
	if cfg.CardPaymentEndpoint == "" {
		// dev / staging 没接 card-payment 时 mock 模式
		logger.Warn("card adapter: card-payment endpoint not configured, running in mock mode")
		return &Adapter{logger: logger, timeout: cfg.RPCTimeout, mocked: true}, nil
	}
	var creds credentials.TransportCredentials
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	} else {
		tc, err := buildTLS(cfg)
		if err != nil {
			return nil, fmt.Errorf("card adapter tls: %w", err)
		}
		creds = credentials.NewTLS(tc)
	}
	conn, err := grpc.NewClient(cfg.CardPaymentEndpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial card-payment: %w", err)
	}
	return &Adapter{
		conn:    conn,
		cli:     cardpaymentv1.NewCardPaymentClient(conn),
		logger:  logger,
		timeout: cfg.RPCTimeout,
	}, nil
}

// Name implements channel.Adapter
func (a *Adapter) Name() string { return "card" }

// Charge 调 card-payment.Authorize 同步等结果。
//
// 关键约束:
//   - req.Metadata["payment_token"] = card-center 颁发的 payment_token
//     (跟 pi_id AAD-bound,TTL 30min)
//   - 本 adapter 自身不见 PAN
//   - card-payment 内部完成 Detokenize → 调卡组织 → 结果回写
func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	if req.PiID == "" || req.IdempotencyKey == "" {
		return nil, fmt.Errorf("card: pi_id / idempotency_key required")
	}
	paymentToken := req.Metadata["payment_token"]
	if paymentToken == "" {
		return nil, fmt.Errorf("card: metadata[payment_token] (= card-center payment_token) required")
	}

	if a.mocked {
		return &channel.ChargeResponse{
			Result:        channel.ResultSucceeded,
			ExternalRefNo: "vmock_" + req.IdempotencyKey,
		}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.cli.Authorize(cctx, &cardpaymentv1.AuthorizeRequest{
		PaymentToken:       paymentToken,
		PiId:               req.PiID,
		Amount:             req.Amount,
		Currency:           req.Currency,
		Network:            req.Metadata["network"], // 空则 card-payment 自判
		MerchantDescriptor: truncate(req.Description, 22),
		TraceId:            req.Metadata["trace_id"],
	})
	if err != nil {
		// 网络/超时 → ResultUnknown 让上层走 Query 推进,而不是 Failed
		return &channel.ChargeResponse{
			Result:         channel.ResultUnknown,
			FailureCode:    "channel_call_error",
			FailureMessage: err.Error(),
		}, fmt.Errorf("card Authorize: %w", err)
	}
	return &channel.ChargeResponse{
		Result:         mapStatusToResult(resp.GetStatus()),
		ExternalRefNo:  resp.GetNetworkRefNo(),
		FailureCode:    resp.GetDeclineCode(),
		RawFailureCode: resp.GetDeclineCode(),
		FailureMessage: resp.GetDeclineReason(),
		Raw: map[string]string{
			"masked_pan": resp.GetMaskedPan(),
			"network":    resp.GetNetwork(),
			"arn":        resp.GetArn(),
		},
	}, nil
}

func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.cli.Capture(cctx, &cardpaymentv1.CaptureRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
		Amount:       req.Amount,
	})
	if err != nil {
		return &channel.OpResponse{Result: channel.ResultUnknown, FailureMessage: err.Error()},
			fmt.Errorf("card Capture: %w", err)
	}
	return &channel.OpResponse{
		Result:        mapStatusToResult(resp.GetStatus()),
		ExternalRefNo: resp.GetNetworkRefNo(),
	}, nil
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.cli.Void(cctx, &cardpaymentv1.VoidRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
	})
	if err != nil {
		return &channel.OpResponse{Result: channel.ResultUnknown, FailureMessage: err.Error()},
			fmt.Errorf("card Void: %w", err)
	}
	return &channel.OpResponse{
		Result:        mapStatusToResult(resp.GetStatus()),
		ExternalRefNo: req.ExternalRefNo,
	}, nil
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: "vrf_" + req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.cli.Refund(cctx, &cardpaymentv1.RefundRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
		Amount:       req.Amount,
		Reason:       req.Reason,
	})
	if err != nil {
		return &channel.OpResponse{Result: channel.ResultUnknown, FailureMessage: err.Error()},
			fmt.Errorf("card Refund: %w", err)
	}
	return &channel.OpResponse{
		Result:        mapStatusToResult(resp.GetStatus()),
		ExternalRefNo: resp.GetRefundRefNo(),
	}, nil
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	if a.mocked {
		return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.ExternalRefNo}, nil
	}
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	resp, err := a.cli.Query(cctx, &cardpaymentv1.QueryRequest{
		NetworkRefNo: req.ExternalRefNo,
		PiId:         req.PiID,
	})
	if err != nil {
		return &channel.QueryResponse{Result: channel.ResultUnknown},
			fmt.Errorf("card Query: %w", err)
	}
	return &channel.QueryResponse{
		Result:         mapStatusToResult(resp.GetStatus()),
		ExternalRefNo:  resp.GetNetworkRefNo(),
		AmountCaptured: resp.GetAmount(),
		Raw: map[string]string{
			"currency":     resp.GetCurrency(),
			"decline_code": resp.GetDeclineCode(),
		},
	}, nil
}

// ParseWebhook 卡支付的 webhook 来源是 card-payment(独立 DC 通过 mTLS 推回);
// 本 adapter 不直接接 Visa / Mastercard webhook(那一层在 card-payment 内部消化)。
//
// payment-channel 暴露 /internal/card-payment/webhook 给 card-payment 服务 POST,
// 走另一条独立路径,这里返回错防止误用。
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	_ = headers
	_ = body
	return nil, errors.New("card adapter: webhook should come from card-payment via internal channel, not direct from network")
}

// Close 关闭 conn
func (a *Adapter) Close() error {
	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────

// mapStatusToResult 把 card-payment status 字符串映射到 channel.ResultType。
//
// card-payment 内部维护的状态机:
//
//	approved        -> succeeded   (Authorize 成功 / Capture 成功)
//	declined        -> failed      (卡组织拒绝,DeclineCode 非空)
//	pending         -> processing  (3DS challenge 中等 / 异步审核)
//	refunded        -> succeeded
//	voided          -> succeeded
//	requires_action -> requires_action
//	unknown / "" / 其它 -> unknown (走 Query 推进)
func mapStatusToResult(s string) channel.ResultType {
	switch strings.ToLower(s) {
	case "approved", "succeeded", "refunded", "voided", "captured":
		return channel.ResultSucceeded
	case "authorized":
		return channel.ResultAuthorized
	case "declined", "failed":
		return channel.ResultFailed
	case "pending", "processing":
		return channel.ResultProcessing
	case "requires_action", "challenge_required":
		return channel.ResultRequiresAction
	default:
		return channel.ResultUnknown
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func buildTLS(cfg Config) (*tls.Config, error) {
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, errors.New("client_cert / client_key required for mTLS")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	out := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if cfg.ServerCA != "" {
		pool := x509.NewCertPool()
		caBytes, err := os.ReadFile(cfg.ServerCA)
		if err != nil {
			return nil, fmt.Errorf("server CA: %w", err)
		}
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("server CA PEM parse failed")
		}
		out.RootCAs = pool
	}
	return out, nil
}

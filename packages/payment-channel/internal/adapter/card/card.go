// Package card 是 payment-channel 的"卡支付"渠道 adapter。
//
// 它**不直接**调 Visa / Mastercard，而是通过 mTLS gRPC 调隔离 DC 内的
// card-payment 服务，由 card-payment 在 SAQ-D 范围内拿 PAN 调卡组织。
//
// 跟现有 15 个 adapter (gcash / maya / ...) 同形：实现 channel.Adapter 接口。
//
// payment-channel 自己**不见 PAN**：传入的 ChargeRequest 里 channel-token 字段
// 应当是 card-center 颁发的 payment_token，本 adapter 只透传给 card-payment。
package card

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

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
	// mTLS 客户端证书（payment-channel 调 card-payment 用）
	ClientCert string
	ClientKey  string
	ServerCA   string
	// dev 用 insecure；prod 必须 false
	Insecure bool
	// RPC 超时
	RPCTimeout time.Duration
}

// Adapter 实现 channel.Adapter
type Adapter struct {
	conn    *grpc.ClientConn
	logger  *zap.Logger
	timeout time.Duration
	mocked  bool // dev / staging 路径无 card-payment 时 short-circuit
}

// New dial card-payment over mTLS（或 insecure for dev）
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
	return &Adapter{conn: conn, logger: logger, timeout: cfg.RPCTimeout}, nil
}

// Name implements channel.Adapter
func (a *Adapter) Name() string { return "card" }

// Charge 把 ChargeRequest 转成 card-payment.AuthorizeRequest，然后等同步结果。
//
// 关键约束：
//   - req.ChannelToken 字段被复用为 card-center 颁发的 payment_token
//     （payment_token 跟 pi_id 绑定，TTL 30min）
//   - 本 adapter 自身不见 PAN
func (a *Adapter) Charge(ctx context.Context, req *channel.ChargeRequest) (*channel.ChargeResponse, error) {
	if req.PiID == "" || req.IdempotencyKey == "" {
		return nil, fmt.Errorf("card: pi_id / idempotency_key required")
	}
	paymentToken := req.ChannelToken
	if paymentToken == "" {
		return nil, fmt.Errorf("card: channel_token (= card-center payment_token) required")
	}

	if a.mocked {
		return &channel.ChargeResponse{
			Result:        channel.ResultSucceeded,
			ExternalRefNo: "vmock_" + req.IdempotencyKey,
			AmountCaptured: req.Amount,
		}, nil
	}

	// TODO: 接通 cardpaymentv1 generated stubs：
	//
	//   cli := cardpaymentv1.NewCardPaymentClient(a.conn)
	//   resp, err := cli.Authorize(cctx, &cardpaymentv1.AuthorizeRequest{
	//       PaymentToken: paymentToken,
	//       PiId:         req.PiID,
	//       Amount:       req.Amount,
	//       Currency:     req.Currency,
	//       Network:      "", // BIN 自动判断
	//       MerchantDescriptor: req.MerchantName,
	//   })
	//   ...
	//
	// 当前 stub 直到 cardpaymentv1 proto 接通：
	cctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()
	_ = cctx
	return nil, errors.New("card adapter: cardpayment proto stubs not wired yet")
}

// Capture / Void / Refund / Query 跟 Charge 同形，先全部走 mock or stub
func (a *Adapter) Capture(ctx context.Context, req *channel.CaptureRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.AcquirerTxID}, nil
	}
	return nil, errors.New("card adapter: capture stub")
}

func (a *Adapter) Void(ctx context.Context, req *channel.VoidRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.AcquirerTxID}, nil
	}
	return nil, errors.New("card adapter: void stub")
}

func (a *Adapter) Refund(ctx context.Context, req *channel.RefundRequest) (*channel.OpResponse, error) {
	if a.mocked {
		return &channel.OpResponse{Result: channel.ResultSucceeded, ExternalRefNo: "vrf_" + req.AcquirerTxID}, nil
	}
	return nil, errors.New("card adapter: refund stub")
}

func (a *Adapter) Query(ctx context.Context, req *channel.QueryRequest) (*channel.QueryResponse, error) {
	if a.mocked {
		return &channel.QueryResponse{Result: channel.ResultSucceeded, ExternalRefNo: req.AcquirerTxID}, nil
	}
	return nil, errors.New("card adapter: query stub")
}

// ParseWebhook 卡支付的 webhook 来源是 card-payment（独立 DC 通过 mTLS 推回）；
// 本 adapter 不直接接 Visa / Mastercard webhook（那一层在 card-payment 内部消化）。
//
// 当前留 stub。card-payment → payment-channel webhook 协议设计 phase 2。
func (a *Adapter) ParseWebhook(headers map[string]string, body []byte) (*channel.WebhookEvent, error) {
	return nil, errors.New("card adapter: webhook should come from card-payment via internal channel, not direct from network")
}

// Close 关闭 conn
func (a *Adapter) Close() error {
	if a.conn != nil {
		return a.conn.Close()
	}
	return nil
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

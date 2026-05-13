// Package cardcenterclient mTLS gRPC 调 card-center 服务（专用于派生支付 token）。
//
// order-core 在 PI Confirm 路径调 CreatePaymentToken：把 user_card 的 stored_token
// 转成绑定 pi_id 的一次性支付 token（TTL 30min），传给 payment-channel.adapter[card]
// → card-payment → card-center.Detokenize → PAN → 卡组织。
//
// 注:cardcenterv1 proto stub 不在本模块直接 import (避免跨服务仓库 build context
// 耦合); 调用方走通用 gRPC ClientConn。要切到强类型,把 cardcenter.pb.go +
// cardcenter_grpc.pb.go vendor 进 packages/order-core/api/proto/cardcenter/v1/
// 并把 import 切回 generated stub 即可。
//
// 当前实现:prod 配置 mTLS 后报"需要 vendor proto stub"显式错误,
// dev / 测试用 NewStub() 路径不受影响 (调用方用 NewStub 注入)。
package cardcenterclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// Client 调 card-center 的 gRPC 客户端
type Client struct {
	conn    *grpc.ClientConn
	timeout time.Duration
}

// Config
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
	ClientCert string
	ClientKey  string
	ServerCA   string
	Insecure   bool
}

// CreatePaymentTokenRequest 业务层请求
type CreatePaymentTokenRequest struct {
	StoredToken string
	UserID      int64
	PIID        string
	Amount      int64
	Currency    string
	TTL         time.Duration
	TraceID     string
}

// CreatePaymentTokenResponse
type CreatePaymentTokenResponse struct {
	PaymentToken string
	ExpiresAt    time.Time
	MaskedPAN    string
	Network      string
}

// New
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cardcenterclient: endpoint required")
	}
	var creds credentials.TransportCredentials
	if cfg.Insecure {
		creds = insecure.NewCredentials()
	} else {
		tc, err := buildTLS(cfg)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tc)
	}
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{conn: conn, timeout: t}, nil
}

// Close
func (c *Client) Close() error { return c.conn.Close() }

// errProtoNotVendored 显式错误:启用真实 card-center 调用前必须把 cardcenterv1
// proto stub vendor 进本模块。绝不静默成功。
var errProtoNotVendored = errors.New(
	"cardcenterclient: cardcenterv1 proto stubs not vendored into order-core " +
		"— see package doc for vendoring instructions")

// CreatePaymentToken 调 card-center.CreatePaymentToken
//
// 当前实现:返回 errProtoNotVendored 强制 prod 部署前 vendor 进 stub;
// dev / 测试由调用方走 mock / stub 注入路径,不经过本函数。
func (c *Client) CreatePaymentToken(ctx context.Context, req *CreatePaymentTokenRequest) (*CreatePaymentTokenResponse, error) {
	if req == nil {
		return nil, errors.New("cardcenterclient: request required")
	}
	if req.StoredToken == "" || req.PIID == "" {
		return nil, errors.New("cardcenterclient: stored_token / pi_id required")
	}
	return nil, errProtoNotVendored
}

func buildTLS(cfg Config) (*tls.Config, error) {
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, errors.New("client_cert / client_key required")
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

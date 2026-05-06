// Package cardcenterclient mTLS gRPC 调 card-center.Detokenize。
//
// **唯一**会拿到 PAN 的客户端。返回的 PAN 立即用、立即清栈。
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

	"github.com/xiongwp/card-payment/internal/processor"
	"github.com/xiongwp/payment-util/serviceregistry"

	cardcenterv1 "github.com/xiongwp/card-center/api/proto/cardcenter/v1"
)

type Client struct {
	conn    *grpc.ClientConn
	api     cardcenterv1.CardCenterClient
	timeout time.Duration
}

type Config struct {
	// Endpoint：静态地址，仅在 RegistryEndpoints 为空时用作 fallback 直连。
	Endpoint string
	// RegistryEndpoints：etcd cluster 地址（如 ["etcd:2379"]）。非空 → 走
	// etcd:///card-center 服务发现（card-center 自注册到 etcd），绕开 docker
	// embedded DNS 把 alias 错绑到不相关容器 IP 的状态机问题。
	RegistryEndpoints []string
	RPCTimeout        time.Duration
	ClientCert        string
	ClientKey         string
	ServerCA          string
	Insecure          bool
}

// New dial card-center over mTLS（或 dev insecure）
//
// 优先 RegistryEndpoints → etcd 服务发现；空时退回 cfg.Endpoint 静态 DNS。
// 至少给一个非空。
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("cardcenterclient: endpoint or registry_endpoints required")
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
	// DialWithFallback：endpoints 非空 → etcd resolver，空 → 退回 endpoint
	// 静态 DNS。两条路径都自动获得 round_robin LB + 10s/3s keepalive +
	// UNAVAILABLE/DEADLINE_EXCEEDED retry。
	conn, err := serviceregistry.DialWithFallback(
		cfg.RegistryEndpoints, "card-center", cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{conn: conn, api: cardcenterv1.NewCardCenterClient(conn), timeout: t}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Detokenize 实现 processor.CardCenter
//
// **注意：本函数返回的 Detokenized.PAN 是真实卡号**。caller (processor.Authorize)
// 必须在 defer 里清栈，绝不能 log，绝不能存。
//
// caller="card-payment" 是 card-center service 层白名单校验项，其它 service 调
// 会被审计 deny。pi_id 进 AAD 防止 token 跨 PI 错绑。
func (c *Client) Detokenize(ctx context.Context, paymentToken, piID string) (*processor.Detokenized, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.api.Detokenize(cctx, &cardcenterv1.DetokenizeRequest{
		PaymentToken: paymentToken,
		PiId:         piID,
		Caller:       "card-payment",
	})
	if err != nil {
		return nil, fmt.Errorf("cardcenterclient Detokenize rpc: %w", err)
	}
	return &processor.Detokenized{
		PAN:        resp.GetPan(),
		ExpMonth:   int(resp.GetExpMonth()),
		ExpYear:    int(resp.GetExpYear()),
		HolderName: resp.GetHolderName(),
		PIID:       piID,
		// Amount/Currency 不来自 card-center —— processor 自己从 PI 填
	}, nil
}

func buildTLS(cfg Config) (*tls.Config, error) {
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, errors.New("cardcenterclient: client_cert / client_key required")
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

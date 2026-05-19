// Package kmsclient wraps the kms-manage Kitex client behind the small
// interface service.KMSClient expects.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (kms-manage) 已同步切.
package kmsclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"
	"github.com/xiongwp/payment-util/kitexutil"

	kmsv1 "github.com/xiongwp/kms-manage/kitex_gen/kms/v1"
	kmsservice "github.com/xiongwp/kms-manage/kitex_gen/kms/v1/kmsservice"

	"github.com/xiongwp/user-merchant-core/internal/metrics"
)

// ErrNotConfigured 没配 endpoint / registry 时构造 Client 返此 sentinel.
var ErrNotConfigured = errors.New("kmsclient: endpoint or registry required")

// Config 拨号配置. Endpoint / RegistryEndpoints 至少一个非空.
type Config struct {
	Endpoint          string
	RegistryEndpoints []string
	BearerToken       string
	RPCTimeout        time.Duration
}

// track 包装 KMS 调用: 记 op + result + duration → Prometheus.
func track(op string, fn func() error) error {
	start := time.Now()
	err := fn()
	result := "success"
	if err != nil {
		result = "error"
	}
	metrics.KMSRPCTotal.WithLabelValues(op, result).Inc()
	metrics.KMSRPCDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	return err
}

// Client adapts kmsservice.Client (Kitex) to service.KMSClient.
type Client struct {
	cli   kmsservice.Client
	token string
	rpcT  time.Duration
}

// New 构造 Kitex client.
//
//	cfg.Endpoint 静态地址 fallback (RegistryEndpoints 为空时)
//	cfg.RegistryEndpoints 非空 → kitexutil etcd resolver
//	cfg.RPCTimeout <=0 默认 5s
//	cfg.BearerToken 非空则每请求自动注入 X-Admin-Token (kitexutil.WithAdminToken)
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, ErrNotConfigured
	}
	rpcT := cfg.RPCTimeout
	if rpcT <= 0 {
		rpcT = 5 * time.Second
	}
	opts := []client.Option{
		client.WithRPCTimeout(rpcT),
		// 强制 gRPC over HTTP/2 over TCP, 避开 Kitex netpoll 把 host:port 当 unix
		// socket 路径解读的 "dial unix ...: no such file or directory" 陷阱.
		client.WithTransportProtocol(transport.GRPC),
	}
	// Kitex etcd resolver 接入留给后续 wire (kitexutil.NewEtcdResolver +
	// client.WithResolver); 目前优先 endpoint 直连, registry 字段已留好.
	if cfg.Endpoint != "" {
		opts = append(opts, client.WithHostPorts(cfg.Endpoint))
	}
	cli, err := kmsservice.NewClient("kms-manage", opts...)
	if err != nil {
		return nil, fmt.Errorf("dial kms-manage (kitex): %w", err)
	}
	return &Client{cli: cli, token: cfg.BearerToken, rpcT: rpcT}, nil
}

// Dial 老接口兼容 (3 参数版本). 推荐用 New(Config{...}).
func Dial(endpoint, bearerToken string, rpcTimeout time.Duration) (*Client, error) {
	return New(Config{Endpoint: endpoint, BearerToken: bearerToken, RPCTimeout: rpcTimeout})
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (c *Client) Close() error { return nil }

func (c *Client) attachToken(ctx context.Context) context.Context {
	if c.token != "" {
		return kitexutil.WithAdminToken(ctx, c.token)
	}
	return ctx
}

// Encrypt satisfies service.KMSClient.
func (c *Client) Encrypt(ctx context.Context, plaintext []byte, aad string) ([]byte, error) {
	var out []byte
	err := track("encrypt", func() error {
		cctx := c.attachToken(ctx)
		resp, err := c.cli.Encrypt(cctx, &kmsv1.EncryptRequest{Plaintext: plaintext, Context: aad})
		if err != nil {
			return err
		}
		out = []byte(resp.GetCiphertext())
		return nil
	})
	return out, err
}

// Decrypt satisfies service.KMSClient.
func (c *Client) Decrypt(ctx context.Context, ciphertext []byte, aad string) ([]byte, error) {
	var out []byte
	err := track("decrypt", func() error {
		cctx := c.attachToken(ctx)
		resp, err := c.cli.Decrypt(cctx, &kmsv1.DecryptRequest{Ciphertext: string(ciphertext), Context: aad})
		if err != nil {
			return err
		}
		out = resp.GetPlaintext()
		return nil
	})
	return out, err
}

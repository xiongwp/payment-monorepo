// Package kmsclient wraps the kms-manage Kitex client behind the small
// interface service.KMSClient expects.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (kms-manage) 已同步切.
package kmsclient

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/xiongwp/payment-util/kitexutil"

	kmsv1 "github.com/xiongwp/kms-manage/kitex_gen/kms/v1"
	kmsservice "github.com/xiongwp/kms-manage/kitex_gen/kms/v1/kmsservice"

	"github.com/xiongwp/user-merchant-core/internal/metrics"
)

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
	cli    kmsservice.Client
	token  string
	rpcT   time.Duration
}

// Dial creates a Kitex client to kms-manage.
//
//	endpoint like "kms-manage:9290"
//	rpcTimeout <=0 defaults to 5s
//	bearerToken 非空则每请求自动注入 X-Admin-Token (kitexutil.WithAdminToken)
func Dial(endpoint, bearerToken string, rpcTimeout time.Duration) (*Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 5 * time.Second
	}
	cli, err := kmsservice.NewClient("kms-manage",
		client.WithHostPorts(endpoint),
		client.WithRPCTimeout(rpcTimeout),
		// 老 grpcutil.Dial 自带 retry 2 次, Kitex 等价: client.WithFailureRetry(retry.NewFailurePolicy())
		// 当前 stub, 真接 Kitex 时取消注释.
		// client.WithFailureRetry(retry.NewFailurePolicy()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial kms-manage (kitex): %w", err)
	}
	return &Client{cli: cli, token: bearerToken, rpcT: rpcTimeout}, nil
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

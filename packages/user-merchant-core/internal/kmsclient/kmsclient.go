// Package kmsclient wraps the kms-manage gRPC client behind the small
// interface service.KMSClient expects. 现在拨号复用 pkg/grpcutil.Dial，
// 自带 keepalive + bearer 注入 + trace 透传 + 可选重试。
package kmsclient

import (
	"context"
	"fmt"
	"time"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	"google.golang.org/grpc"

	"github.com/xiongwp/user-merchant-core/internal/metrics"
	"github.com/xiongwp/user-merchant-core/pkg/grpcutil"
	"github.com/xiongwp/user-merchant-core/pkg/tracex"
)

// track 包装 KMS 调用：记录 op + result + duration，喂给 Prometheus。
// result 只分 success/error 两档；线上定位要 code 粒度再补 label。
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

// Client adapts kmsv1.KMSServiceClient to service.KMSClient.
type Client struct {
	conn *grpc.ClientConn
	cli  kmsv1.KMSServiceClient
}

// Dial creates a gRPC client to kms-manage.
//   endpoint like "kms-manage:9290"
//   rpcTimeout <=0 defaults to 5s
//   bearerToken 非空则每请求自动注入 authorization: Bearer <...>
func Dial(endpoint, bearerToken string, rpcTimeout time.Duration) (*Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 5 * time.Second
	}
	conn, err := grpcutil.Dial(grpcutil.ClientDialOptions{
		Endpoint:         endpoint,
		Timeout:          rpcTimeout,
		MaxRetries:       2, // Encrypt / Decrypt 是幂等的，short retry 是安全的
		BearerToken:      bearerToken,
		TraceMetadataKey: tracex.MetadataKey,
	})
	if err != nil {
		return nil, fmt.Errorf("dial kms-manage: %w", err)
	}
	return &Client{conn: conn, cli: kmsv1.NewKMSServiceClient(conn)}, nil
}

// Close 关闭连接。优雅停机时 main 会调。
func (c *Client) Close() error { return c.conn.Close() }

// Encrypt satisfies service.KMSClient.
func (c *Client) Encrypt(ctx context.Context, plaintext []byte, aad string) ([]byte, error) {
	var out []byte
	err := track("encrypt", func() error {
		resp, err := c.cli.Encrypt(ctx, &kmsv1.EncryptRequest{Plaintext: plaintext, Context: aad})
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
		resp, err := c.cli.Decrypt(ctx, &kmsv1.DecryptRequest{Ciphertext: string(ciphertext), Context: aad})
		if err != nil {
			return err
		}
		out = resp.GetPlaintext()
		return nil
	})
	return out, err
}

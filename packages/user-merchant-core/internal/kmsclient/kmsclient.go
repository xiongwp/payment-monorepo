// Package kmsclient — 临时 STUB.
//
// 原版通过 Kitex 调 kms-manage.Encrypt/Decrypt. cross-service kitex_gen 还没接进
// docker build 流程 (additional_contexts + replace), 暂改 stub:
//
//   - Dial 不真拨号, 返回带 timeout 的 *Client.
//   - Encrypt/Decrypt 永远返 ErrNotWired, 调用方需走降级 (本地 mock / passthrough)
//     或拒绝处理 PII (生产配置须显式关 KMS pipeline).
//
// 接通 sibling sourcing 后, 应恢复 kmsservice.NewClient + cli.Encrypt/Decrypt 真实调用.
package kmsclient

import (
	"context"
	"errors"
	"time"

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

// ErrNotWired stub 返此 sentinel — 调用方据此走降级.
var ErrNotWired = errors.New("kmsclient STUB: kms-manage kitex_gen not wired in build")

// Client stub: 不持有真 Kitex client, 仅记 timeout / token 保签名兼容.
type Client struct {
	token string
	rpcT  time.Duration
}

// Dial 构造 stub client. 不做 endpoint dial.
func Dial(_, bearerToken string, rpcTimeout time.Duration) (*Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 5 * time.Second
	}
	return &Client{token: bearerToken, rpcT: rpcTimeout}, nil
}

// Close no-op.
func (c *Client) Close() error { return nil }

// Encrypt stub: 永远返 ErrNotWired (经 track 计入 metrics).
func (c *Client) Encrypt(_ context.Context, _ []byte, _ string) ([]byte, error) {
	err := track("encrypt", func() error { return ErrNotWired })
	return nil, err
}

// Decrypt stub: 永远返 ErrNotWired (经 track 计入 metrics).
func (c *Client) Decrypt(_ context.Context, _ []byte, _ string) ([]byte, error) {
	err := track("decrypt", func() error { return ErrNotWired })
	return nil, err
}

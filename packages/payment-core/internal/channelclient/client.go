// Package channelclient 把 payment-channel 的 AcquirerService Kitex API 包一层,
// 暴露为本仓 service 层友好的方法签名. 所有重试 / 熔断只做最表层的超时控制 ——
// 幂等与重放完全由 payment-channel 保证.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (payment-channel) 已同步切.
// mTLS 不需要 (内部 mesh 明文).
package channelclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"
	"github.com/xiongwp/payment-util/kitexutil"

	channelv1 "github.com/xiongwp/payment-channel/kitex_gen/channel/v1"
	acquirerservice "github.com/xiongwp/payment-channel/kitex_gen/channel/v1/acquirerservice"
)

// normalizeGRPCTarget strips leading "grpc://" / "http://" if someone pasted
// a URL into the endpoint config, and trims surrounding whitespace.
func normalizeGRPCTarget(s string) string {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"grpc://", "http://", "https://", "tcp://"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	return s
}

// Client 面向 service 层的门面.
type Client interface {
	Charge(ctx context.Context, in *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error)
	Capture(ctx context.Context, in *channelv1.CaptureRequest) (*channelv1.OpResponse, error)
	Void(ctx context.Context, in *channelv1.VoidRequest) (*channelv1.OpResponse, error)
	Refund(ctx context.Context, in *channelv1.RefundRequest) (*channelv1.OpResponse, error)
	Query(ctx context.Context, in *channelv1.QueryRequest) (*channelv1.QueryResponse, error)
	Close() error
}

type kitexClient struct {
	api  acquirerservice.Client
	rpcT time.Duration
}

// Dial 建立到 payment-channel 的 Kitex 连接.
//
// 老 gRPC 时代有的功能 (per-method retry policy / keepalive / 8MB max msg
// size / mTLS / trace+shadow interceptor) 切 Kitex 后行为对应映射:
//   - Retry policy → client.WithFailureRetry / client.WithBackupRequest, 暂留 TODO
//   - Keepalive → Kitex 默认内置, 不用手动配
//   - Max msg size → 默认 4MB, 超大 webhook 用 client.WithGRPCInitialWindowSize
//   - mTLS → 已废 (内部 mesh)
//   - trace / shadow → 待 kitexutil.TraceMW / ShadowMW port
//
// registry 非空 → kitexutil.EtcdResolver; 空 → 直连 endpoint.
func Dial(registry []string, endpoint string, rpcTimeout time.Duration) (Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 10 * time.Second
	}
	endpoint = normalizeGRPCTarget(endpoint)
	const serviceName = "payment-channel"

	// ETCD-5: 默认走 kitexutil.DefaultClientOptions (REGISTRY_ENDPOINTS 非空 → etcd
	// discovery, 否则静态 docker DNS). 显式 endpoint 仍可覆盖.
	opts := kitexutil.DefaultClientOptions(serviceName)
	opts = append(opts,
		client.WithRPCTimeout(rpcTimeout),
		client.WithTransportProtocol(transport.GRPC),
		// TODO: per-method retry policy — Charge/Capture/Void/Refund 仅 UNAVAILABLE; Query +DEADLINE_EXCEEDED
		// TODO: shadow + trace MW (port 老 grpc interceptor)
	)
	if endpoint != "" {
		opts = append(opts, client.WithHostPorts(endpoint))
	}
	_ = registry // legacy param; 由 REGISTRY_ENDPOINTS env 替代

	api, err := acquirerservice.NewClient(serviceName, opts...)
	if err != nil {
		return nil, fmt.Errorf("channelclient kitex dial: %w", err)
	}
	return &kitexClient{api: api, rpcT: rpcTimeout}, nil
}

func (c *kitexClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.rpcT)
}

func (c *kitexClient) Charge(ctx context.Context, in *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Charge(ctx, in)
}
func (c *kitexClient) Capture(ctx context.Context, in *channelv1.CaptureRequest) (*channelv1.OpResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Capture(ctx, in)
}
func (c *kitexClient) Void(ctx context.Context, in *channelv1.VoidRequest) (*channelv1.OpResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Void(ctx, in)
}
func (c *kitexClient) Refund(ctx context.Context, in *channelv1.RefundRequest) (*channelv1.OpResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Refund(ctx, in)
}
func (c *kitexClient) Query(ctx context.Context, in *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Query(ctx, in)
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (c *kitexClient) Close() error { return nil }

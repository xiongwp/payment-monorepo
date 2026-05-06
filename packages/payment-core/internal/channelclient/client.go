// Package channelclient 把 payment-channel 的 AcquirerService gRPC API 包一层，
// 暴露为本仓 service 层友好的方法签名。所有重试 / 熔断只做最表层的超时控制 ——
// 幂等与重放完全由 payment-channel 保证。
package channelclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	channelv1 "github.com/xiongwp/payment-channel/api/proto/channel/v1"
	"github.com/xiongwp/payment-util/mtls"
	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/shadow"

	"github.com/xiongwp/payment-core/internal/trace"
)

// normalizeGRPCTarget strips leading "grpc://" / "http://" if someone pasted
// a URL into the endpoint config, and trims surrounding whitespace.
//
// The "name resolver error: produced zero addresses" crash typically hits
// when gRPC receives a malformed target like "grpc://host:9092" (gRPC treats
// the whole thing as a scheme and looks up nothing), an empty string, or a
// target with stray whitespace from yaml quoting.
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

// Client 面向 service 层的门面。
type Client interface {
	Charge(ctx context.Context, in *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error)
	Capture(ctx context.Context, in *channelv1.CaptureRequest) (*channelv1.OpResponse, error)
	Void(ctx context.Context, in *channelv1.VoidRequest) (*channelv1.OpResponse, error)
	Refund(ctx context.Context, in *channelv1.RefundRequest) (*channelv1.OpResponse, error)
	Query(ctx context.Context, in *channelv1.QueryRequest) (*channelv1.QueryResponse, error)
	Close() error
}

type grpcClient struct {
	conn *grpc.ClientConn
	api  channelv1.AcquirerServiceClient
	rpcT time.Duration
}

// Dial 建立到 payment-channel 的 gRPC 连接。endpoint 形如 "127.0.0.1:9092"。
//
// 连接参数针对 payment-core 的高 QPS + 长连接场景做了调优：
//   - Keepalive：每 30s 发一次 PING，若 10s 内无 ACK 就重连；保证穿过 L4 LB
//     的连接不会被空闲 kill。
//   - 连接重试退避：初始 500ms、封顶 10s，避免 payment-channel 短暂抖动时
//     打出大量重连风暴。
//   - Service config：Charge / Refund / Query 全部可重试 UNAVAILABLE 两次；
//     payment-channel 侧的 UNIQUE(adapter, idempotency_key) 保证重放安全。
//   - 最大消息体 8MB：webhook 原始报文偶尔会很大。
//
// registry 非空 → etcd resolver；空 → 直连 endpoint。两条路都 round_robin LB。
func Dial(registry []string, endpoint string, rpcTimeout time.Duration) (Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 10 * time.Second
	}
	endpoint = normalizeGRPCTarget(endpoint)
	if endpoint == "" && len(registry) == 0 {
		return nil, fmt.Errorf("channelclient.Dial: endpoint and registry both empty")
	}
	const serviceName = "payment-channel"
	// mTLS 条件接入
	mtlsCfg, mtlsErr := mtls.LoadFromEnv()
	if mtlsErr != nil {
		return nil, fmt.Errorf("channelclient.Dial: mtls config: %w", mtlsErr)
	}
	var creds grpc.DialOption
	if mtlsCfg.InsecureDev || (mtlsCfg.ServerCertPath == "" && mtlsCfg.ServerKeyPath == "" && mtlsCfg.CACertPath == "") {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		tlsCreds, cerr := mtlsCfg.ClientCredentials()
		if cerr != nil {
			return nil, fmt.Errorf("channelclient.Dial: load mTLS creds: %w", cerr)
		}
		creds = grpc.WithTransportCredentials(tlsCreds)
	}
	conn, err := serviceregistry.DialWithFallback(registry, serviceName, endpoint,
		creds,
		// trace + shadow 都需要透传到下游 payment-channel
		grpc.WithChainUnaryInterceptor(
			trace.UnaryClientInterceptor(),
			shadow.UnaryClientInterceptor(),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  500 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   10 * time.Second,
			},
			MinConnectTimeout: 2 * time.Second,
		}),
		// Retry 策略按方法分类：
		//   - Query 是只读：DEADLINE_EXCEEDED + UNAVAILABLE 都安全重试。
		//   - Charge / Capture / Void / Refund 是 mutation：只 retry UNAVAILABLE
		//     （连接级错误，请求未到下游）；**绝不** retry DEADLINE_EXCEEDED——
		//     deadline 触发时下游可能已经收到请求并在处理，重试会造成重复落账。
		//     超时由 service 层捕获后转为 ResultProcessing+result_unknown 上报，
		//     由 order-core / 对账主动 Query 兜底。
		grpc.WithDefaultServiceConfig(`{
  "methodConfig": [
    {
      "name": [
        {"service": "channel.v1.AcquirerService", "method": "Charge"},
        {"service": "channel.v1.AcquirerService", "method": "Capture"},
        {"service": "channel.v1.AcquirerService", "method": "Void"},
        {"service": "channel.v1.AcquirerService", "method": "Refund"}
      ],
      "retryPolicy": {
        "maxAttempts": 2,
        "initialBackoff": "0.2s",
        "maxBackoff": "2s",
        "backoffMultiplier": 2.0,
        "retryableStatusCodes": ["UNAVAILABLE"]
      }
    },
    {
      "name": [{"service": "channel.v1.AcquirerService", "method": "Query"}],
      "retryPolicy": {
        "maxAttempts": 3,
        "initialBackoff": "0.2s",
        "maxBackoff": "2s",
        "backoffMultiplier": 2.0,
        "retryableStatusCodes": ["UNAVAILABLE", "DEADLINE_EXCEEDED"]
      }
    }
  ]
}`),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(8<<20),
			grpc.MaxCallSendMsgSize(8<<20),
		),
	)
	if err != nil {
		return nil, err
	}
	return &grpcClient{
		conn: conn,
		api:  channelv1.NewAcquirerServiceClient(conn),
		rpcT: rpcTimeout,
	}, nil
}

func (c *grpcClient) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.rpcT)
}

func (c *grpcClient) Charge(ctx context.Context, in *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Charge(ctx, in)
}
func (c *grpcClient) Capture(ctx context.Context, in *channelv1.CaptureRequest) (*channelv1.OpResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Capture(ctx, in)
}
func (c *grpcClient) Void(ctx context.Context, in *channelv1.VoidRequest) (*channelv1.OpResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Void(ctx, in)
}
func (c *grpcClient) Refund(ctx context.Context, in *channelv1.RefundRequest) (*channelv1.OpResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Refund(ctx, in)
}
func (c *grpcClient) Query(ctx context.Context, in *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.api.Query(ctx, in)
}
func (c *grpcClient) Close() error { return c.conn.Close() }

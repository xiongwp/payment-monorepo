// Package kmsclient 把 kms-manage 的 KMSService gRPC API 包一层，暴露给 payment-core
// 的 secret 包用。设计对齐本仓 channelclient：同样的 keepalive / 重试策略，
// 同一套超时语义，配置键前缀 kms.*。
package kmsclient

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	"github.com/xiongwp/payment-util/mtls"
	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/shadow"

	"github.com/xiongwp/payment-core/internal/trace"
)

// Client 是 payment-core 这一层对 kms-manage 的门面，只暴露 Encrypt / Decrypt。
// 不暴露 GenerateDataKey（当前 payment-core 没 envelope 需求）。
type Client interface {
	Encrypt(ctx context.Context, plaintext []byte, aadContext string) (string, error)
	Decrypt(ctx context.Context, ciphertext, aadContext string) ([]byte, error)
	Close() error
}

// NoopClient 在未配置 kms 时使用：直接原样 roundtrip，密文字段必须是明文。
// 用于单测 / 本地开发。
type NoopClient struct{}

func (NoopClient) Encrypt(_ context.Context, plaintext []byte, _ string) (string, error) {
	return string(plaintext), nil
}
func (NoopClient) Decrypt(_ context.Context, ciphertext, _ string) ([]byte, error) {
	return []byte(ciphertext), nil
}
func (NoopClient) Close() error { return nil }

type grpcClient struct {
	conn  *grpc.ClientConn
	api   kmsv1.KMSServiceClient
	rpcT  time.Duration
	token string // 可空
}

// Dial 建立到 kms-manage 的 gRPC 连接。keepalive/重试策略和 channelclient.Dial 对齐。
//
// registry 非空 → etcd resolver（联栈多 pod 部署必走）；空 → 直连 endpoint（dev / 单仓）。
// 两条路径都用 round_robin LB 在多副本间均摊。
//
// mTLS 模式：MTLS_SERVER_CERT/KEY/CA 配了 → 使用 mTLS credentials；
// 缺配或 INSECURE_DIAL=1（dev only）→ insecure mode。
func Dial(registry []string, endpoint, bearerToken string, rpcTimeout time.Duration) (Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 3 * time.Second
	}

	// Load mTLS config; fail-fast in production if certs missing
	mtlsCfg, err := mtls.LoadFromEnv()
	if err != nil {
		return nil, err
	}

	var creds grpc.DialOption
	if mtlsCfg.InsecureDev || (mtlsCfg.ServerCertPath == "" && mtlsCfg.ServerKeyPath == "" && mtlsCfg.CACertPath == "") {
		// Dev/test mode: no mTLS certs configured
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	} else {
		// mTLS mode: load credentials
		tlsCreds, cerr := mtlsCfg.ClientCredentials()
		if cerr != nil {
			return nil, cerr
		}
		creds = grpc.WithTransportCredentials(tlsCreds)
	}

	const serviceName = "kms-manage"
	conn, err := serviceregistry.DialWithFallback(registry, serviceName, endpoint,
		creds,
		// trace + shadow 都透传到 kms-manage：kms 可对 shadow 流量返 mock 密文
		// 或走影子路径（按 kms-manage 自身实现），这里负责标识。
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
		grpc.WithDefaultServiceConfig(`{
  "methodConfig": [{
    "name": [{"service": "kms.v1.KMSService"}],
    "retryPolicy": {
      "maxAttempts": 3,
      "initialBackoff": "0.1s",
      "maxBackoff": "1s",
      "backoffMultiplier": 2.0,
      "retryableStatusCodes": ["UNAVAILABLE", "DEADLINE_EXCEEDED"]
    }
  }]
}`),
	)
	if err != nil {
		return nil, err
	}
	return &grpcClient{
		conn:  conn,
		api:   kmsv1.NewKMSServiceClient(conn),
		rpcT:  rpcTimeout,
		token: strings.TrimSpace(bearerToken),
	}, nil
}

func (c *grpcClient) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, c.rpcT)
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	return ctx, cancel
}

func (c *grpcClient) Encrypt(ctx context.Context, plaintext []byte, aadContext string) (string, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	out, err := c.api.Encrypt(ctx, &kmsv1.EncryptRequest{Plaintext: plaintext, Context: aadContext})
	if err != nil {
		return "", err
	}
	return out.GetCiphertext(), nil
}

func (c *grpcClient) Decrypt(ctx context.Context, ciphertext, aadContext string) ([]byte, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	out, err := c.api.Decrypt(ctx, &kmsv1.DecryptRequest{Ciphertext: ciphertext, Context: aadContext})
	if err != nil {
		return nil, err
	}
	return out.GetPlaintext(), nil
}

func (c *grpcClient) Close() error { return c.conn.Close() }

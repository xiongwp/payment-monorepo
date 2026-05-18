// Package kmsclient 把 kms-manage 的 KMSService Kitex API 包一层, 暴露给 payment-core
// 的 secret 包用. 切 Kitex 后跟 gRPC wire 不互通; server side 已同步切.
package kmsclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/xiongwp/payment-util/kitexutil"

	kmsv1 "reconcile-system/packages/kms-manage/kitex_gen/kms/v1"
	kmsservice "reconcile-system/packages/kms-manage/kitex_gen/kms/v1/kmsservice"
)

// Client 是 payment-core 这一层对 kms-manage 的门面, 只暴露 Encrypt / Decrypt.
type Client interface {
	Encrypt(ctx context.Context, plaintext []byte, aadContext string) (string, error)
	Decrypt(ctx context.Context, ciphertext, aadContext string) ([]byte, error)
	Close() error
}

// NoopClient 在未配置 kms 时使用: 直接原样 roundtrip, 密文字段必须是明文.
// 用于单测 / 本地开发.
type NoopClient struct{}

func (NoopClient) Encrypt(_ context.Context, plaintext []byte, _ string) (string, error) {
	return string(plaintext), nil
}
func (NoopClient) Decrypt(_ context.Context, ciphertext, _ string) ([]byte, error) {
	return []byte(ciphertext), nil
}
func (NoopClient) Close() error { return nil }

type kitexClient struct {
	api   kmsservice.Client
	rpcT  time.Duration
	token string
}

// Dial 建立到 kms-manage 的 Kitex 连接.
//
// registry 非空 → kitexutil.EtcdResolver, Kitex 自动 round_robin LB;
// 空 → 直连 endpoint (dev / 单仓).
//
// 内部 service mesh 不走 mTLS (按用户决策); 边缘网关单向 TLS 在 ingress 层做.
func Dial(registry []string, endpoint, bearerToken string, rpcTimeout time.Duration) (Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 3 * time.Second
	}
	opts := []client.Option{
		client.WithRPCTimeout(rpcTimeout),
		client.WithHostPorts(endpoint),
		// TODO: shadow + trace MW (port 老 grpc shadow.UnaryClientInterceptor / trace.UnaryClientInterceptor)
	}
	if len(registry) > 0 {
		// TODO: opts = append(opts, client.WithResolver(kitexutil.NewEtcdResolver(etcdCli, "")))
		_ = registry
	}

	const serviceName = "kms-manage"
	api, err := kmsservice.NewClient(serviceName, opts...)
	if err != nil {
		return nil, fmt.Errorf("kmsclient kitex dial: %w", err)
	}
	return &kitexClient{
		api:   api,
		rpcT:  rpcTimeout,
		token: strings.TrimSpace(bearerToken),
	}, nil
}

func (c *kitexClient) ctx(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, c.rpcT)
	if c.token != "" {
		ctx = kitexutil.WithAdminToken(ctx, c.token)
	}
	return ctx, cancel
}

func (c *kitexClient) Encrypt(ctx context.Context, plaintext []byte, aadContext string) (string, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	out, err := c.api.Encrypt(ctx, &kmsv1.EncryptRequest{Plaintext: plaintext, Context: aadContext})
	if err != nil {
		return "", err
	}
	return out.GetCiphertext(), nil
}

func (c *kitexClient) Decrypt(ctx context.Context, ciphertext, aadContext string) ([]byte, error) {
	ctx, cancel := c.ctx(ctx)
	defer cancel()
	out, err := c.api.Decrypt(ctx, &kmsv1.DecryptRequest{Ciphertext: ciphertext, Context: aadContext})
	if err != nil {
		return nil, err
	}
	return out.GetPlaintext(), nil
}

// Close — Kitex 内部 connection pool 自动管理, no-op 兼容老接口.
func (c *kitexClient) Close() error { return nil }

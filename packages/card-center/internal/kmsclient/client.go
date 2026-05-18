// Package kmsclient 调 kms-manage 做 envelope 加解密 — Kitex (Protobuf IDL).
//
// 实现 vault.KMS 接口. 切 Kitex 后 wire 协议跟 gRPC 不互通,
// kms-manage server side 已同步切 (idl/kms/v1/kms.proto + kitex_gen).
package kmsclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/xiongwp/payment-util/kitexutil"

	kmsv1 "reconcile-system/packages/kms-manage/kitex_gen/kms/v1"
	kmsservice "reconcile-system/packages/kms-manage/kitex_gen/kms/v1/kmsservice"
)

// Client kms-manage Kitex 客户端.
//
// Kitex client 自己管理 connection pool + LB + retry; 不再像 gRPC 那样
// 持有一条 *grpc.ClientConn.
type Client struct {
	api     kmsservice.Client
	timeout time.Duration
	bearer  string
}

// Config 客户端配置.
type Config struct {
	// Endpoint: 静态地址 (host:port), 仅在 RegistryEndpoints 为空时用作 fallback.
	Endpoint string
	// RegistryEndpoints: etcd cluster 地址列表, 非空时优先走 kitexutil.EtcdResolver.
	RegistryEndpoints []string
	BearerToken       string
	RPCTimeout        time.Duration
	// mTLS (Kitex 通过 client.WithTransportProtocol + tls.Config 配; 当前 stub 不接 TLS).
	ClientCert string
	ClientKey  string
	ServerCA   string
	// dev 路径允许 insecure; prod assertProdSafety 会拒.
	Insecure bool
}

// New dial kms-manage via Kitex.
//
// 优先级:
//   - RegistryEndpoints 非空 → kitexutil.EtcdResolver, Kitex client 自动 LB
//   - RegistryEndpoints 空 → fallback 直连 cfg.Endpoint
//
// 至少给一个非空, 否则起不来.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("kmsclient: endpoint or registry_endpoints required")
	}
	opts := []client.Option{
		// 跟历史 retry / keepalive 参数对齐 (Kitex 等价配置, 真实接 Kitex 时取消注释)
		// client.WithRPCTimeout(7 * time.Second),
		// client.WithConnectTimeout(3 * time.Second),
	}

	// 服务发现 — etcd 优先, fallback 直连.
	// TODO: 接真实 etcd cli 后注入 kitexutil.NewEtcdResolver; 当前 stub.
	if len(cfg.RegistryEndpoints) > 0 {
		// resolver, err := buildEtcdResolver(cfg.RegistryEndpoints)
		// opts = append(opts, client.WithResolver(resolver))
		_ = cfg.RegistryEndpoints
	} else {
		opts = append(opts, client.WithHostPorts(cfg.Endpoint))
	}

	// mTLS — Kitex 用 tls.Config + client.WithTransportProtocol(transport.GRPC) (兼容模式)
	// 或者 client.WithTLS(tlsCfg) (纯 TTHeader 模式). 当前 stub 走 insecure.
	// TODO: 接 buildTLS(cfg) 后 opts = append(opts, client.WithTLSConfig(tlsCfg))

	api, err := kmsservice.NewClient("kms-manage", opts...)
	if err != nil {
		return nil, fmt.Errorf("kmsclient kitex dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		// 7s: HSM-backed KMS P99 < 5s, 留 2s 余量
		t = 7 * time.Second
	}
	return &Client{
		api:     api,
		timeout: t,
		bearer:  cfg.BearerToken,
	}, nil
}

// Close Kitex client 内部 connection pool 自动管理; 这里 no-op 兼容老接口.
func (c *Client) Close() error { return nil }

// Encrypt 实现 vault.KMS — 映射到 kms-manage Kitex Encrypt RPC.
//
// AAD 在 proto 字段名是 context (vault 包里叫 aad, 语义一致).
// 返 ciphertext 形如 "kms:v1:<key_id>:<base64-payload>".
func (c *Client) Encrypt(ctx context.Context, plaintext []byte, aad string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.bearer != "" {
		cctx = kitexutil.WithAdminToken(cctx, c.bearer)
	}
	resp, err := c.api.Encrypt(cctx, &kmsv1.EncryptRequest{
		Plaintext: plaintext,
		Context:   aad,
		// KeyId 留空 → kms-manage 用 keystore.active
	})
	if err != nil {
		return "", "", fmt.Errorf("kmsclient Encrypt rpc: %w", err)
	}
	return resp.GetCiphertext(), resp.GetKeyId(), nil
}

// Decrypt 实现 vault.KMS — 映射到 kms-manage Kitex Decrypt RPC.
//
// AAD 必须跟 Encrypt 时**完全一致**; 不一致会被 kms-manage 拒.
func (c *Client) Decrypt(ctx context.Context, ciphertext string, aad string) ([]byte, string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.bearer != "" {
		cctx = kitexutil.WithAdminToken(cctx, c.bearer)
	}
	resp, err := c.api.Decrypt(cctx, &kmsv1.DecryptRequest{
		Ciphertext: ciphertext,
		Context:    aad,
	})
	if err != nil {
		return nil, "", fmt.Errorf("kmsclient Decrypt rpc: %w", err)
	}
	return resp.GetPlaintext(), resp.GetKeyId(), nil
}

// Package kmsclient 调 kms-manage（隔离 DC 内的独立实例）做 envelope 加解密。
//
// 实现 vault.KMS 接口。
package kmsclient

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
	"google.golang.org/grpc/metadata"

	kmsv1 "github.com/xiongwp/kms-manage/api/proto/kms/v1"
	"github.com/xiongwp/payment-util/serviceregistry"
)

// Client kms-manage gRPC 客户端（mTLS）
type Client struct {
	conn    *grpc.ClientConn
	api     kmsv1.KMSServiceClient
	timeout time.Duration
	bearer  string
}

// Config 客户端配置
type Config struct {
	Endpoint          string
	RegistryEndpoints []string // 非空走 etcd:///kms-manage 服务发现
	BearerToken string
	RPCTimeout  time.Duration
	// mTLS 客户端证书（card DC 内 service-to-service mutual auth）
	ClientCert string
	ClientKey  string
	ServerCA   string
	// dev 路径允许 insecure；prod assertProdSafety 会拒
	Insecure bool
	// BypassHardened：临时旁路，跳过 serviceregistry hardened opts 直接 grpc.NewClient。
	// 仅用于排查（service config / keepalive 等是否引发卡 RPC）。
	BypassHardened bool
}

// New dial kms-manage
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("kmsclient: endpoint or registry_endpoints required")
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
	var (
		conn *grpc.ClientConn
		err  error
	)
	if cfg.BypassHardened {
		// 旁路模式：直接 grpc.NewClient(endpoint, creds)，不走 etcd resolver、
		// 不附 service config、不挂 keepalive。绑卡 KMS 调用是低频，stale conn
		// 的风险换 RPC 必到，先保跑通。
		conn, err = grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
	} else {
		conn, err = serviceregistry.DialWithFallback(
			cfg.RegistryEndpoints, "kms-manage", cfg.Endpoint,
			grpc.WithTransportCredentials(creds),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("kmsclient dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		// 7s：HSM-backed KMS P99 一般 < 5s，留 2s 余量；3s 容易 false-fail
		t = 7 * time.Second
	}
	return &Client{conn: conn, api: kmsv1.NewKMSServiceClient(conn), timeout: t, bearer: cfg.BearerToken}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Encrypt 调 kms-manage.KMSService.Encrypt。AAD 在 proto 字段名是 `context`
// （vault 包里我们叫 aad，语义一致）。返回 ciphertext 形如
// `kms:v1:<key_id>:<base64-payload>`，作为 stored / payment token 内容。
func (c *Client) Encrypt(ctx context.Context, plaintext []byte, aad string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.bearer != "" {
		cctx = metadata.AppendToOutgoingContext(cctx, "authorization", "Bearer "+c.bearer)
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

// Decrypt 调 kms-manage.KMSService.Decrypt。AAD 必须跟 Encrypt 时**完全一致**；
// 不一致会被 kms-manage 拒（防止 token 错绑用户/PI）。
func (c *Client) Decrypt(ctx context.Context, ciphertext string, aad string) ([]byte, string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.bearer != "" {
		cctx = metadata.AppendToOutgoingContext(cctx, "authorization", "Bearer "+c.bearer)
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

// buildTLS 从 cfg 加载客户端 cert + 信任 server CA
func buildTLS(cfg Config) (*tls.Config, error) {
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, errors.New("kmsclient: client_cert / client_key required for mTLS")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	out := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
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

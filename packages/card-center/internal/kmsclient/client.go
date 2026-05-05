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
)

// Client kms-manage gRPC 客户端（mTLS）
type Client struct {
	conn    *grpc.ClientConn
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
	conn, err := serviceregistry.DialWithFallback(
		cfg.RegistryEndpoints, "kms-manage", cfg.Endpoint,
		grpc.WithTransportCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("kmsclient dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 3 * time.Second
	}
	return &Client{conn: conn, timeout: t, bearer: cfg.BearerToken}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Encrypt 实现 vault.KMS。
//
// 注意：本 stub 假设 kms-manage 的 gRPC 客户端代码（kmsv1.KMSServiceClient）
// 已经由 protoc 生成。card-center 真要 build 时需要 import kms-manage 的
// generated proto 包，这里先用占位结构体让编译通过；后续补完。
func (c *Client) Encrypt(ctx context.Context, plaintext []byte, aad string) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.bearer != "" {
		cctx = metadata.AppendToOutgoingContext(cctx, "authorization", "Bearer "+c.bearer)
	}
	// TODO: 调真实 kmsv1 client：
	//   resp, err := kmsv1.NewKMSServiceClient(c.conn).Encrypt(cctx,
	//       &kmsv1.EncryptRequest{Plaintext: plaintext, Aad: aad})
	//   return resp.Ciphertext, resp.KeyId, err
	//
	// 本 stub 等 kms-manage proto import 接通后替换。
	_ = cctx
	return "", "", errors.New("kmsclient: TODO wire kmsv1 generated stubs")
}

// Decrypt 实现 vault.KMS
func (c *Client) Decrypt(ctx context.Context, ciphertext string, aad string) ([]byte, string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if c.bearer != "" {
		cctx = metadata.AppendToOutgoingContext(cctx, "authorization", "Bearer "+c.bearer)
	}
	// TODO: 同上
	_ = cctx
	return nil, "", errors.New("kmsclient: TODO wire kmsv1 generated stubs")
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

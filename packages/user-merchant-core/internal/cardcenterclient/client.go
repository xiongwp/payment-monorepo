// Package cardcenterclient mTLS gRPC 调 card-center 服务。
//
// 用途：用户存卡 / 删卡 / 查询某 token 状态时调 card-center。
// 派生支付 token 不在这一侧（在 order-core 创建 PI confirm 时调）。
package cardcenterclient

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
)

// Client 调 card-center 的 gRPC 客户端
type Client struct {
	conn    *grpc.ClientConn
	timeout time.Duration
}

// Config 客户端配置
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
	ClientCert string
	ClientKey  string
	ServerCA   string
	Insecure   bool // dev 用 insecure；prod 必须 mTLS
}

// **PAN 单跳后 Tokenize 已退役**（task #81）：浏览器 → card-center HTTPS 直连
// → user-merchant-core 只接 stored_token（已 KMS 加密）。本服务进程**完全不
// touch PAN**。原 TokenizeRequest / TokenizeResponse 类型 + Tokenize 方法已
// 删除，留下这段注释作为历史足迹防止后人重新加回去。
//
// 如果你看到这里想 "我加个 Tokenize 多方便" —— 不要。任何写 PAN 字段的代码
// 都会让 user-merchant-core 进 SAQ-D scope，相当于把 PCI 合规半径扩大三倍。
// PAN 流转走 card-center HTTPS 单跳，只此一条路径。

// New dial
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cardcenterclient: endpoint required")
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
	conn, err := grpc.NewClient(cfg.Endpoint, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{conn: conn, timeout: t}, nil
}

// Close 关连接
func (c *Client) Close() error { return c.conn.Close() }

// DeleteCard 调 card-center.DeleteCard（业务层 soft delete）
func (c *Client) DeleteCard(ctx context.Context, userID int64, storedToken, reason, traceID string) error {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_ = cctx
	// TODO: cli.DeleteCard(...)
	return errors.New("cardcenterclient: TODO wire cardcenterv1 stubs")
}

func buildTLS(cfg Config) (*tls.Config, error) {
	if cfg.ClientCert == "" || cfg.ClientKey == "" {
		return nil, errors.New("client_cert / client_key required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("client keypair: %w", err)
	}
	out := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
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

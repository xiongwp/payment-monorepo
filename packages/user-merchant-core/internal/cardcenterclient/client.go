// Package cardcenterclient — Kitex client to card-center.
//
// user-merchant-core 在 UserCardService.DeleteCard 软删 user_card 后, best-effort
// 异步调 card-center.DeleteCard 把 stored_token 标 revoked. 失败只 log 不阻断.
package cardcenterclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"
	cardcenterv1 "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1"
	cardcenterservice "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1/cardcenter"
	"github.com/xiongwp/payment-util/kitexutil"
)

// ErrNotConfigured 构造时没给 endpoint / registry 时返此 sentinel.
var ErrNotConfigured = errors.New("cardcenterclient: endpoint or registry required")

// Config 拨号配置. Endpoint 静态 fallback, RegistryEndpoints 走 etcd resolver.
type Config struct {
	Endpoint          string
	RegistryEndpoints []string
	BearerToken       string
	RPCTimeout        time.Duration
}

// Client wraps cardcenterservice.Client.
type Client struct {
	cli     cardcenterservice.Client
	token   string
	timeout time.Duration
}

// New 构造 Kitex client to card-center.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, ErrNotConfigured
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	opts := []client.Option{
		client.WithRPCTimeout(t),
		// 强制 gRPC over HTTP/2 over TCP, 避开 Kitex netpoll 把 host:port 当 unix
		// socket 路径解读的 "dial unix ...: no such file or directory" 陷阱.
		client.WithTransportProtocol(transport.GRPC),
	}
	// Kitex etcd resolver 接入留给后续 wire; 目前优先 endpoint 直连.
	if cfg.Endpoint != "" {
		opts = append(opts, client.WithHostPorts(cfg.Endpoint))
	}
	cli, err := cardcenterservice.NewClient("card-center", opts...)
	if err != nil {
		return nil, fmt.Errorf("dial card-center (kitex): %w", err)
	}
	return &Client{cli: cli, token: cfg.BearerToken, timeout: t}, nil
}

// Close — Kitex 自带 connection pool, no-op.
func (c *Client) Close() error { return nil }

// DeleteCard 通知 card-center 把 stored_token 标 revoked.
// 参数顺序与历史 stub 保持一致 (userID, storedToken, lastFour, reason).
// lastFour 字段 cardcenter.proto 里没有, 这里仅用于历史 caller 兼容, 实际不传给 RPC.
func (c *Client) DeleteCard(ctx context.Context, userID int64, storedToken, lastFour, reason string) error {
	_ = lastFour
	if storedToken == "" {
		return errors.New("stored_token required")
	}
	if c.token != "" {
		ctx = kitexutil.WithAdminToken(ctx, c.token)
	}
	_, err := c.cli.DeleteCard(ctx, &cardcenterv1.DeleteCardRequest{
		UserId:      fmt.Sprintf("%d", userID),
		StoredToken: storedToken,
		Reason:      reason,
	})
	return err
}

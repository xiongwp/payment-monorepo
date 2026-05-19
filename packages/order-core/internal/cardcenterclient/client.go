// Package cardcenterclient — 临时 STUB.
//
// 原版通过 Kitex 调 card-center.CreatePaymentToken. cross-service kitex_gen
// 还没在 docker build 流程里 wire 进去, 暂改 stub: 永远返 ErrNotWired,
// 调用方走降级路径 (业务上 card 支付暂不可用).
package cardcenterclient

import (
	"context"
	"errors"
	"time"
)

type Client struct{ timeout time.Duration }

type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
}

type CreatePaymentTokenRequest struct {
	StoredToken string
	UserID      int64
	PIID        string
	Amount      int64
	Currency    string
	TTL         time.Duration
	TraceID     string
}

type CreatePaymentTokenResponse struct {
	PaymentToken string
	ExpiresAt    time.Time
	MaskedPAN    string
	Network      string
}

// ErrNotWired stub 模式返此 sentinel, caller 走降级.
var ErrNotWired = errors.New("cardcenterclient STUB: card-center kitex_gen not wired in build")

func New(cfg Config) (*Client, error) {
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{timeout: t}, nil
}

func (c *Client) Close() error { return nil }

func (c *Client) CreatePaymentToken(_ context.Context, _ *CreatePaymentTokenRequest) (*CreatePaymentTokenResponse, error) {
	return nil, ErrNotWired
}

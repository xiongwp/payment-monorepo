// Package cardcenterclient — Kitex client to card-center.CreatePaymentToken.
//
// order-core 在 PaymentIntent.Confirm 时, 卡支付路径会先调 card-center.
// CreatePaymentToken 把 stored_token 兑换成单笔 payment_token (绑定 pi_id),
// 然后把 payment_token 转给 card-payment 真扣款.
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

// ErrNotConfigured Endpoint 为空时构造返此 sentinel.
var ErrNotConfigured = errors.New("cardcenterclient: endpoint required")

// Config 拨号配置.
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
}

// Client wraps cardcenterservice.Client.
type Client struct {
	cli     cardcenterservice.Client
	timeout time.Duration
}

// CreatePaymentTokenRequest 业务侧入参 (本仓 domain layer 形态, 不直接是 proto).
type CreatePaymentTokenRequest struct {
	StoredToken string
	UserID      int64
	PIID        string
	Amount      int64
	Currency    string
	TTL         time.Duration
	TraceID     string
}

// CreatePaymentTokenResponse 业务侧响应.
type CreatePaymentTokenResponse struct {
	PaymentToken string
	ExpiresAt    time.Time
	MaskedPAN    string
	Network      string
}

// New 构造 Kitex client. cfg.Endpoint 为空 → 走 etcd discovery.
func New(cfg Config) (*Client, error) {
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	// ETCD-5: kitexutil.DefaultClientOptions 自动按 REGISTRY_ENDPOINTS 切 etcd / 静态.
	opts := kitexutil.DefaultClientOptions("card-center")
	opts = append(opts,
		client.WithTransportProtocol(transport.GRPC),
		client.WithRPCTimeout(t),
	)
	if cfg.Endpoint != "" {
		opts = append(opts, client.WithHostPorts(cfg.Endpoint))
	}
	cli, err := cardcenterservice.NewClient("card-center", opts...)
	if err != nil {
		return nil, fmt.Errorf("dial card-center: %w", err)
	}
	return &Client{cli: cli, timeout: t}, nil
}

// Close — Kitex 自带 connection pool, no-op.
func (c *Client) Close() error { return nil }

// CreatePaymentToken 拨号 card-center.CreatePaymentToken; 返回业务侧 response 结构.
func (c *Client) CreatePaymentToken(ctx context.Context, in *CreatePaymentTokenRequest) (*CreatePaymentTokenResponse, error) {
	if in == nil || in.StoredToken == "" {
		return nil, errors.New("stored_token required")
	}
	ttlSec := int32(in.TTL / time.Second)
	if ttlSec <= 0 || ttlSec > 1800 {
		ttlSec = 1800
	}
	resp, err := c.cli.CreatePaymentToken(ctx, &cardcenterv1.CreatePaymentTokenRequest{
		StoredToken: in.StoredToken,
		UserId:      fmt.Sprintf("%d", in.UserID),
		PiId:        in.PIID,
		Amount:      in.Amount,
		Currency:    in.Currency,
		TtlSeconds:  ttlSec,
		TraceId:     in.TraceID,
	})
	if err != nil {
		return nil, err
	}
	return &CreatePaymentTokenResponse{
		PaymentToken: resp.GetPaymentToken(),
		ExpiresAt:    time.Unix(resp.GetExpiresAt(), 0),
		MaskedPAN:    resp.GetMaskedPan(),
		Network:      resp.GetNetwork(),
	}, nil
}

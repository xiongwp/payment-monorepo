// Package cardcenterclient Kitex 调 card-center 服务 (派生支付 token).
//
// order-core 在 PI Confirm 路径调 CreatePaymentToken: 把 user_card 的 stored_token
// 转成绑定 pi_id 的一次性支付 token (TTL 30min), 传给 payment-channel.adapter[card]
// → card-payment → card-center.Detokenize → PAN → 卡组织.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (card-center) 已同步切.
// mTLS 已不需要 (内部 mesh 明文).
package cardcenterclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"

	cardcenterv1 "reconcile-system/packages/card-center/kitex_gen/cardcenter/v1"
	cardcenterservice "reconcile-system/packages/card-center/kitex_gen/cardcenter/v1/cardcenterservice"
)

// Client 调 card-center 的 Kitex 客户端
type Client struct {
	api     cardcenterservice.Client
	timeout time.Duration
}

// Config
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
}

// CreatePaymentTokenRequest 业务层请求
type CreatePaymentTokenRequest struct {
	StoredToken string
	UserID      int64
	PIID        string
	Amount      int64
	Currency    string
	TTL         time.Duration
	TraceID     string
}

// CreatePaymentTokenResponse
type CreatePaymentTokenResponse struct {
	PaymentToken string
	ExpiresAt    time.Time
	MaskedPAN    string
	Network      string
}

// New 建立 Kitex 客户端.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cardcenterclient: endpoint required")
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	api, err := cardcenterservice.NewClient("card-center",
		client.WithHostPorts(cfg.Endpoint),
		client.WithRPCTimeout(t),
	)
	if err != nil {
		return nil, fmt.Errorf("kitex dial: %w", err)
	}
	return &Client{api: api, timeout: t}, nil
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (c *Client) Close() error { return nil }

// CreatePaymentToken 调 card-center.CreatePaymentToken Kitex RPC.
func (c *Client) CreatePaymentToken(ctx context.Context, req *CreatePaymentTokenRequest) (*CreatePaymentTokenResponse, error) {
	if req == nil {
		return nil, errors.New("cardcenterclient: request required")
	}
	if req.StoredToken == "" || req.PIID == "" {
		return nil, errors.New("cardcenterclient: stored_token / pi_id required")
	}
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ttlSec := int32(req.TTL.Seconds())
	if ttlSec <= 0 {
		ttlSec = 1800 // 30 min 默认 TTL
	}
	resp, err := c.api.CreatePaymentToken(cctx, &cardcenterv1.CreatePaymentTokenRequest{
		StoredToken: req.StoredToken,
		UserId:      req.UserID,
		PiId:        req.PIID,
		Amount:      req.Amount,
		Currency:    req.Currency,
		TtlSeconds:  ttlSec,
		TraceId:     req.TraceID,
	})
	if err != nil {
		return nil, fmt.Errorf("card-center CreatePaymentToken rpc: %w", err)
	}
	return &CreatePaymentTokenResponse{
		PaymentToken: resp.GetPaymentToken(),
		ExpiresAt:    time.Unix(resp.GetExpiresAt(), 0),
		MaskedPAN:    resp.GetMaskedPan(),
		Network:      resp.GetNetwork(),
	}, nil
}

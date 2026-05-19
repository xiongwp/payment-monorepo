// Package usermerchantclient — Kitex client to user-merchant-core.UserCardInternalService.
//
// order-core 在 PaymentIntent.Confirm 卡路径凭 (user_id, user_card_id) 拿 stored_token,
// 转给 card-center.CreatePaymentToken 兑成 payment_token. 是内部 RPC, listen 在
// user-merchant-core 的 internal port (受 mTLS + clientCN 白名单保护).
package usermerchantclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/transport"
	usermerchantv1 "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1"
	usercardinternalservice "github.com/xiongwp/user-merchant-core/kitex_gen/usermerchant/v1/usercardinternalservice"
)

// CardLookup 接口 — 给 order-core PI Confirm 路径用.
type CardLookup interface {
	GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (storedToken, maskedPAN, network string, err error)
}

// Config 拨号配置. mTLS 字段保留兼容 (Kitex transport 层暂未接通 mTLS, 但配置
// 接口保持稳定方便后续接入 kitexutil.MTLSClientCredentials).
type Config struct {
	Endpoint   string
	RPCTimeout time.Duration
	ClientCert string
	ClientKey  string
	ServerCA   string
	Insecure   bool
}

// ErrNotConfigured Endpoint 为空时构造返此 sentinel.
var ErrNotConfigured = errors.New("usermerchantclient: endpoint required")

// kitexLookup 真 Kitex 实现.
type kitexLookup struct {
	cli     usercardinternalservice.Client
	timeout time.Duration
}

// New 构造真 Kitex client. Endpoint 为空时直接报错 (上游 dev 模式应该传 NewStub).
func New(cfg Config) (CardLookup, error) {
	if cfg.Endpoint == "" {
		return nil, ErrNotConfigured
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 3 * time.Second
	}
	cli, err := usercardinternalservice.NewClient("user-merchant-core",
		client.WithHostPorts(cfg.Endpoint),
		client.WithTransportProtocol(transport.GRPC),		client.WithRPCTimeout(t),
	)
	if err != nil {
		return nil, fmt.Errorf("dial user-merchant-core internal: %w", err)
	}
	return &kitexLookup{cli: cli, timeout: t}, nil
}

func (k *kitexLookup) GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (string, string, string, error) {
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	resp, err := k.cli.GetStoredTokenForPayment(ctx, &usermerchantv1.GetStoredTokenRequest{
		UserId:     userID,
		UserCardId: userCardID,
	})
	if err != nil {
		return "", "", "", err
	}
	return resp.GetStoredToken(), resp.GetMaskedPan(), resp.GetNetwork(), nil
}

// stubLookup 给单测用 — 返回固定假值.
type stubLookup struct{ storedToken, maskedPAN, network string }

// NewStub 给单测用.
func NewStub(storedToken, maskedPAN, network string) CardLookup {
	return &stubLookup{storedToken: storedToken, maskedPAN: maskedPAN, network: network}
}

func (s *stubLookup) GetStoredTokenForPayment(_ context.Context, userID, userCardID int64) (string, string, string, error) {
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	if s.storedToken == "" {
		return "", "", "", errors.New("usermerchantclient: stub not configured (test only)")
	}
	return s.storedToken, s.maskedPAN, s.network, nil
}

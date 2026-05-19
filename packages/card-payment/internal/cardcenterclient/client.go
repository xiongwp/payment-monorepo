// Package cardcenterclient Kitex 调 card-center.Detokenize.
//
// **唯一**会拿到 PAN 的客户端. 返回的 PAN 立即用、立即清栈.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side 已同步切. mTLS 不需要 (内部 mesh).
package cardcenterclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/kitex/client"

	cardcenterv1 "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1"
	cardcenterservice "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1/cardcenter"

	"github.com/xiongwp/card-payment/internal/processor"
)

type Client struct {
	api     cardcenterservice.Client
	timeout time.Duration
}

type Config struct {
	// Endpoint: 静态地址, 仅在 RegistryEndpoints 为空时用作 fallback 直连.
	Endpoint string
	// RegistryEndpoints: etcd cluster 地址. 非空 → 走 kitexutil.EtcdResolver, Kitex
	// 自动 round_robin LB.
	RegistryEndpoints []string
	RPCTimeout        time.Duration
}

// New dial card-center over Kitex.
//
// 优先 RegistryEndpoints → etcd 服务发现; 空时退回 cfg.Endpoint 静态 DNS.
// 至少给一个非空.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" && len(cfg.RegistryEndpoints) == 0 {
		return nil, errors.New("cardcenterclient: endpoint or registry_endpoints required")
	}
	t := cfg.RPCTimeout
	if t <= 0 {
		t = 5 * time.Second
	}
	opts := []client.Option{
		client.WithRPCTimeout(t),
		client.WithHostPorts(cfg.Endpoint),
	}
	// TODO: 接 etcd 后 opts = append(opts, client.WithResolver(kitexutil.NewEtcdResolver(etcdCli, "")))
	_ = cfg.RegistryEndpoints

	api, err := cardcenterservice.NewClient("card-center", opts...)
	if err != nil {
		return nil, fmt.Errorf("kitex dial: %w", err)
	}
	return &Client{api: api, timeout: t}, nil
}

// Close — Kitex 自带 connection pool, no-op 兼容老接口.
func (c *Client) Close() error { return nil }

// Detokenize 实现 processor.CardCenter.
//
// **注意: 本函数返回的 Detokenized.PAN 是真实卡号**. caller (processor.Authorize)
// 必须在 defer 里清栈, 绝不能 log, 绝不能存.
//
// caller="card-payment" 是 card-center service 层白名单校验项, 其它 service 调
// 会被审计 deny. pi_id 进 AAD 防止 token 跨 PI 错绑.
func (c *Client) Detokenize(ctx context.Context, paymentToken, piID string) (*processor.Detokenized, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.api.Detokenize(cctx, &cardcenterv1.DetokenizeRequest{
		PaymentToken: paymentToken,
		PiId:         piID,
		Caller:       "card-payment",
	})
	if err != nil {
		return nil, fmt.Errorf("cardcenterclient Detokenize rpc: %w", err)
	}
	return &processor.Detokenized{
		PAN:        resp.GetPan(),
		ExpMonth:   int(resp.GetExpMonth()),
		ExpYear:    int(resp.GetExpYear()),
		HolderName: resp.GetHolderName(),
		PIID:       piID,
	}, nil
}

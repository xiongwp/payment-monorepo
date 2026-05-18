// Package riskclient 把 risk-manage 的 RiskService Kitex API 包一层.
// payment-core 在路由到 payment-channel 之前调 Screen() 做风控判定.
//
// 切 Kitex 后跟 gRPC wire 不互通; server side (risk-manage) 已同步切.
package riskclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/kitex/client"

	riskv1 "reconcile-system/packages/risk-manage/kitex_gen/risk/v1"
	riskservice "reconcile-system/packages/risk-manage/kitex_gen/risk/v1/riskservice"
)

// Decision 三态
type Decision int

const (
	Allow  Decision = 1
	Deny   Decision = 2
	Review Decision = 3
)

// ScreenResult 风控判定结果
type ScreenResult struct {
	Decision  Decision
	Reason    string
	RiskScore int
	RiskLevel string
	// DecisionID 和 risk-manage audit / review queue 一致；payment-core 把它
	// 写到 webhook + log，让商户客服 / dispute 系统能反查"为什么这笔被 review"。
	DecisionID string
	// RecommendedAction risk-manage 给的动作建议（block / step_up_3ds / ""）。
	// payment-core 决定是否执行（取决于商户能力 / 配置），不强制听从。
	RecommendedAction string
}

// Client 风控客户端接口
type Client interface {
	Screen(ctx context.Context, req *ScreenRequest) (*ScreenResult, error)
	Report(ctx context.Context, req *ReportRequest) error
	Close() error
}

type ScreenRequest struct {
	PaymentIntentID string
	MerchantID      string
	CustomerID      string
	Amount          int64
	Currency        string
	PaymentMethod   string
	Country         string
	IPAddress       string
	DeviceID        string
	UserAgent       string
	// RiskSessionID checkout 阶段 web/mobile SDK 创建的 session id；
	// risk-manage 内部用 it 查 SessionStore 把 fingerprint + behavior 字段补上。
	RiskSessionID string
	// IdempotencyKey 同 key 60s 内 Screen 复用上次结果，建议用 PaymentIntentID。
	IdempotencyKey string
}

type ReportRequest struct {
	PaymentIntentID string
	MerchantID      string
	CustomerID      string
	Amount          int64
	Currency        string
	PaymentMethod   string
	EventType       string // payment.succeeded / payment.failed / refund.succeeded
	FailureCode     string
	IPAddress       string
	DeviceID        string
}

// NoopClient 未配置 risk endpoint 时使用：全部放行。
type NoopClient struct{}

func (NoopClient) Screen(_ context.Context, _ *ScreenRequest) (*ScreenResult, error) {
	return &ScreenResult{Decision: Allow, RiskLevel: "low"}, nil
}
func (NoopClient) Report(_ context.Context, _ *ReportRequest) error { return nil }
func (NoopClient) Close() error                                     { return nil }

// kitexClient 真实 Kitex 客户端
type kitexClient struct {
	api  riskservice.Client
	rpcT time.Duration
}

// Dial 连到 risk-manage Kitex.
//
// registry 非空 → kitexutil.EtcdResolver, Kitex 自动 round_robin LB;
// 空 → 退回 endpoint 直连 (dev / 单仓).
func Dial(registry []string, endpoint string, rpcTimeout time.Duration) (Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 3 * time.Second
	}
	// Strip scheme prefix / whitespace.
	endpoint = strings.TrimSpace(endpoint)
	for _, p := range []string{"grpc://", "http://", "https://", "tcp://"} {
		if strings.HasPrefix(endpoint, p) {
			endpoint = strings.TrimPrefix(endpoint, p)
			break
		}
	}
	if endpoint == "" && len(registry) == 0 {
		return nil, fmt.Errorf("riskclient.Dial: endpoint and registry both empty")
	}
	const serviceName = "risk-manage"
	// mTLS check (prod fail-fast if certs 缺); Kitex 当前 stub 不接 TLS, 待 mtls.KitexTLSConfig 完成.
	mtlsCfg, mtlsErr := mtls.LoadFromEnv()
	if mtlsErr != nil {
		return nil, fmt.Errorf("riskclient.Dial: mtls config: %w", mtlsErr)
	}
	_ = mtlsCfg // TODO: 接 mtls.KitexTLSConfig 后 opts = append(opts, client.WithTLSConfig(...))

	opts := []client.Option{
		client.WithRPCTimeout(rpcTimeout),
		client.WithHostPorts(endpoint),
		// TODO: 接 etcd resolver — client.WithResolver(kitexutil.NewEtcdResolver(etcdCli, ""))
		// TODO: shadow MW (risk-manage 看到 x-shadow=1 短路 ALLOW)
		// TODO: trace MW
	}
	_ = registry // TODO: etcd resolver

	api, err := riskservice.NewClient(serviceName, opts...)
	if err != nil {
		return nil, fmt.Errorf("riskclient kitex dial: %w", err)
	}
	return &kitexClient{api: api, rpcT: rpcTimeout}, nil
}

func (c *kitexClient) Screen(ctx context.Context, req *ScreenRequest) (*ScreenResult, error) {
	ctx, cancel := context.WithTimeout(ctx, c.rpcT)
	defer cancel()
	resp, err := c.api.Screen(ctx, &riskv1.ScreenRequest{
		PaymentIntentId: req.PaymentIntentID,
		MerchantId:      req.MerchantID,
		CustomerId:      req.CustomerID,
		Amount:          req.Amount,
		Currency:        req.Currency,
		PaymentMethod:   req.PaymentMethod,
		Country:         req.Country,
		IpAddress:       req.IPAddress,
		DeviceId:        req.DeviceID,
		UserAgent:       req.UserAgent,
		RiskSessionId:   req.RiskSessionID,
		IdempotencyKey:  req.IdempotencyKey,
	})
	if err != nil {
		return nil, err
	}
	return &ScreenResult{
		Decision:          Decision(resp.GetDecision()),
		Reason:            resp.GetReason(),
		RiskScore:         int(resp.GetRiskScore()),
		RiskLevel:         resp.GetRiskLevel(),
		DecisionID:        resp.GetDecisionId(),
		RecommendedAction: resp.GetRecommendedAction(),
	}, nil
}

func (c *kitexClient) Report(ctx context.Context, req *ReportRequest) error {
	ctx, cancel := context.WithTimeout(ctx, c.rpcT)
	defer cancel()
	_, err := c.api.Report(ctx, &riskv1.ReportRequest{
		PaymentIntentId: req.PaymentIntentID,
		MerchantId:      req.MerchantID,
		CustomerId:      req.CustomerID,
		Amount:          req.Amount,
		Currency:        req.Currency,
		PaymentMethod:   req.PaymentMethod,
		EventType:       req.EventType,
		FailureCode:     req.FailureCode,
		IpAddress:       req.IPAddress,
		DeviceId:        req.DeviceID,
	})
	return err
}

// Close — Kitex 内部 connection pool 自动管理, no-op 兼容老接口.
func (c *kitexClient) Close() error { return nil }

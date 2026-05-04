// Package riskclient 把 risk-manage 的 RiskService gRPC API 包一层。
// payment-core 在路由到 payment-channel 之前调 Screen() 做风控判定。
package riskclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	riskv1 "github.com/xiongwp/risk-manage/api/proto/risk/v1"
	"github.com/xiongwp/payment-util/serviceregistry"
	"github.com/xiongwp/payment-util/shadow"

	"github.com/xiongwp/payment-core/internal/trace"
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

// grpcClient 真实 gRPC 客户端
type grpcClient struct {
	conn *grpc.ClientConn
	api  riskv1.RiskServiceClient
	rpcT time.Duration
}

// Dial 连到 risk-manage gRPC。
//
// registry 非空 → 走 etcd resolver（联栈多 pod 必走，因为容器去掉 container_name 后
// "risk-manage" 跨 compose 项目 DNS 解析不到）；为空 → 退回 endpoint 直连。
// 两条路径都用 round_robin LB 在多副本间均摊。
func Dial(registry []string, endpoint string, rpcTimeout time.Duration) (Client, error) {
	if rpcTimeout <= 0 {
		rpcTimeout = 3 * time.Second
	}
	// Strip scheme prefix / whitespace — same class of yaml-quoting /
	// grpc:// URL mistake that caused channel dial to fail with
	// "produced zero addresses".
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
	conn, err := serviceregistry.DialWithFallback(registry, serviceName, endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// risk-manage 看到 x-shadow=1 会直接 ALLOW（短路放行），不消耗风控资源
		grpc.WithChainUnaryInterceptor(
			trace.UnaryClientInterceptor(),
			shadow.UnaryClientInterceptor(),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true,
		}),
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay: 500 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 10 * time.Second,
			},
			MinConnectTimeout: 2 * time.Second,
		}),
	)
	if err != nil {
		return nil, err
	}
	return &grpcClient{conn: conn, api: riskv1.NewRiskServiceClient(conn), rpcT: rpcTimeout}, nil
}

func (c *grpcClient) Screen(ctx context.Context, req *ScreenRequest) (*ScreenResult, error) {
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

func (c *grpcClient) Report(ctx context.Context, req *ReportRequest) error {
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

func (c *grpcClient) Close() error { return c.conn.Close() }

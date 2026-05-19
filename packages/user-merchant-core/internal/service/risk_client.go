// Package service — risk-manage Kitex client interface (STUB).
//
// 临时降级：risk-manage 的 kitex_gen 还没接进 docker build (cross-service
// additional_contexts 链未打通)，本仓用本地 stub 类型镜像 riskv1 的字段/枚举 shape,
// 让 register / login / change_password 调用 risk.Screen / risk.Report 路径编译通过。
// NoopRiskClient 全 ALLOW + no-op，业务上等同 "风控降级" (fail-open)。
//
// 等 Dockerfile multi-stage build 把 risk-manage/kitex_gen 拉进来后,
// 这个文件应该改回 import riskv1 + 真实 grpc client。
package service

import "context"

// Decision 风控决策枚举 (mirror risk.Decision)。
type Decision int32

const (
	Decision_ALLOW  Decision = 1
	Decision_REVIEW Decision = 2
	Decision_DENY   Decision = 3
)

// ─── ScreenRequest / ScreenResponse ───────────────────────────────────────

// ScreenRequest 镜像 riskv1.ScreenRequest 仅 user-merchant-core 用到的字段。
type ScreenRequest struct {
	EventType       string
	MerchantId      string
	CustomerId      string
	IpAddress       string
	DeviceId        string
	UserAgent       string
	RiskSessionId   string
	FingerprintHash string
	IdempotencyKey  string
	Metadata        map[string]string
}

// ScreenResponse 镜像 riskv1.ScreenResponse.
type ScreenResponse struct {
	Decision   Decision
	DecisionId string
	Reason     string
	RiskLevel  string
}

// ─── ReportRequest ────────────────────────────────────────────────────────

// ReportRequest 镜像 riskv1.ReportRequest.
type ReportRequest struct {
	EventType  string
	MerchantId string
	CustomerId string
	IpAddress  string
	DeviceId   string
}

// ─── client interface + Noop 实现 ─────────────────────────────────────────

// RiskClient 仅暴露 user-merchant-core 用到的两个方法。
type RiskClient interface {
	Screen(ctx context.Context, req *ScreenRequest) (*ScreenResponse, error)
	Report(ctx context.Context, req *ReportRequest) error
}

// NoopRiskClient 没配 risk endpoint / stub 阶段用：Screen 全 ALLOW, Report no-op.
type NoopRiskClient struct{}

func (NoopRiskClient) Screen(_ context.Context, _ *ScreenRequest) (*ScreenResponse, error) {
	return &ScreenResponse{
		Decision:  Decision_ALLOW,
		RiskLevel: "low",
	}, nil
}

func (NoopRiskClient) Report(_ context.Context, _ *ReportRequest) error { return nil }

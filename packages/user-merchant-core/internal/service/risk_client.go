// Package service — risk-manage gRPC client interface。
//
// user-merchant-core 在 register / login / password_change / bind_phone /
// 2fa.disable 等事件先调 risk.Screen 决策，再 risk.Report 写图边。
//
// 实现：grpcRiskClient 在 main.go 装填；NoopRiskClient dev / 单测兜底。
package service

import (
	"context"

	riskv1 "github.com/xiongwp/risk-manage/kitex_gen/risk/v1"
)

// RiskClient 仅暴露 user-merchant-core 用到的两个方法。
type RiskClient interface {
	Screen(ctx context.Context, req *riskv1.ScreenRequest) (*riskv1.ScreenResponse, error)
	Report(ctx context.Context, req *riskv1.ReportRequest) error
}

// NoopRiskClient 没配 risk endpoint 时用：Screen 全 ALLOW，Report no-op。
type NoopRiskClient struct{}

func (NoopRiskClient) Screen(_ context.Context, _ *riskv1.ScreenRequest) (*riskv1.ScreenResponse, error) {
	return &riskv1.ScreenResponse{
		Decision:  riskv1.Decision_ALLOW,
		RiskLevel: "low",
	}, nil
}

func (NoopRiskClient) Report(_ context.Context, _ *riskv1.ReportRequest) error { return nil }

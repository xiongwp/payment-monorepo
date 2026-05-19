// Package service — accounting-system gRPC client interface。
//
// Register 流程在 CreateUser 之后调 Accounting.CreateAccount 开 USER_BALANCE
// 主账户，再把返回的 account_no 写到 user_accounts 表（绑定）。
//
// 失败容错：调 accounting 报错只 warn 不阻断注册（CLAUDE.md 约定 "首次支付时
// 兜底重试"）。
//
// 实现：grpcAccountingClient 在 main.go 装填；NoopAccountingClient dev / 单测兜底。
package service

import (
	"context"

	accv1 "github.com/xiongwp/accounting-system/kitex_gen/accounting/v1"
)

// AccountingClient 仅暴露 user-merchant-core 用到的 CreateAccount。
type AccountingClient interface {
	CreateAccount(ctx context.Context, req *accv1.CreateAccountRequest) (*accv1.CreateAccountResponse, error)
}

// NoopAccountingClient 没配 endpoint 时用：返回空账户，绑定步骤会被 service
// skip（caller 用 GetAccountNo() == "" 判别）。
type NoopAccountingClient struct{}

func (NoopAccountingClient) CreateAccount(_ context.Context, _ *accv1.CreateAccountRequest) (*accv1.CreateAccountResponse, error) {
	return &accv1.CreateAccountResponse{Code: 0, Message: "noop"}, nil
}

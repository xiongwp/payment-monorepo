// Package service — accounting-system Kitex client interface (STUB).
//
// 临时降级：accounting-system 的 kitex_gen 还没接进 docker build (cross-service
// additional_contexts 链未打通)，本仓用本地 stub 类型镜像 accv1 的字段/枚举 shape，
// 让 register / openUserBalanceAccount 编译通过。NoopAccountingClient 永远返回
// 空账户 → caller (UserService.openUserBalanceAccount) 看 AccountNo=="" 直接 skip
// 绑定 (首次支付兜底重试)。
//
// 等 Dockerfile multi-stage build 把 accounting-system/kitex_gen 拉进来后,
// 这个文件应该改回 import accv1 + 真实 grpc client。
package service

import "context"

// ─── 本地枚举常量 (mirror accounting-system proto) ─────────────────────────

// AccountType 用户/平台账户类型。
type AccountType int32

const (
	AccountType_ACCOUNT_TYPE_USER AccountType = 1
)

// AccountCategory 资产/负债分类。
type AccountCategory int32

const (
	AccountCategory_ACCOUNT_CATEGORY_LIABILITY AccountCategory = 2
)

// AccountBusinessType 业务用途。
type AccountBusinessType int32

const (
	AccountBusinessType_ACCOUNT_BUSINESS_TYPE_USER_BALANCE AccountBusinessType = 1
)

// ─── 本地 message struct (mirror accv1 子集) ──────────────────────────────

// CreateAccountRequest 镜像 accv1.CreateAccountRequest 仅 user-merchant-core 用到的字段。
type CreateAccountRequest struct {
	UserId              int64
	AccountType         AccountType
	Category            AccountCategory
	Currency            string
	Description         string
	AccountBusinessType AccountBusinessType
}

// Account 镜像 accv1.Account.
type Account struct {
	AccountNo string
}

// GetAccountNo getter — 保持调用方 nil-safe 语法 (resp.GetAccount().GetAccountNo())。
func (a *Account) GetAccountNo() string {
	if a == nil {
		return ""
	}
	return a.AccountNo
}

// CreateAccountResponse 镜像 accv1.CreateAccountResponse.
type CreateAccountResponse struct {
	Code    int32
	Message string
	Account *Account
}

// GetAccount nil-safe getter.
func (r *CreateAccountResponse) GetAccount() *Account {
	if r == nil {
		return nil
	}
	return r.Account
}

// GetCode nil-safe getter.
func (r *CreateAccountResponse) GetCode() int32 {
	if r == nil {
		return 0
	}
	return r.Code
}

// GetMessage nil-safe getter.
func (r *CreateAccountResponse) GetMessage() string {
	if r == nil {
		return ""
	}
	return r.Message
}

// ─── client interface + Noop 实现 ─────────────────────────────────────────

// AccountingClient 仅暴露 user-merchant-core 用到的 CreateAccount。
type AccountingClient interface {
	CreateAccount(ctx context.Context, req *CreateAccountRequest) (*CreateAccountResponse, error)
}

// NoopAccountingClient 没配 endpoint 或 stub 阶段用：返回空账户，绑定步骤会被
// service skip (caller 看 GetAccountNo()=="" 判别)。
type NoopAccountingClient struct{}

func (NoopAccountingClient) CreateAccount(_ context.Context, _ *CreateAccountRequest) (*CreateAccountResponse, error) {
	return &CreateAccountResponse{Code: 0, Message: "noop"}, nil
}

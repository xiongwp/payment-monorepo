// Package clients — accounting-system 客户端的 stub.
//
// SP-AC-7 之后, split-payment 业务路径(graph save + multi-leg 落账)走的是
// 同包内的 AccountingGRPCClient (accounting_grpc.go) → 新 gRPC TransactionService.
//
// 这个文件原本是用 accountingv1 proto 类型的 *AccountingClient* 实现, 但用了
// 不存在的 proto 字段 (Direction / AccountId / Amount on AccountingEntry,
// BUSINESS_TYPE_SPLIT_PAYMENT enum) — 历史 aspirational 代码, 从未编译过.
//
// 现在保留为 stub: 方法签名跟 workflow 包里 Engine.Accounting 引用的契约一致,
// 但调用一律返 "not wired" 错误. 这条 PostMovements / PostSplitAtomic / ReverseSplit /
// GetBalance 路径被 SP-AC-7 的 multi-leg gRPC 路径取代, 没 caller 真触发.
// 如果有 caller(Kafka subscriber 等) 误调到, 会在日志里看到明确错误而不是
// 神秘 segfault.
package clients

import (
	"context"
	"errors"

	"reconcile-system/packages/split-payment/internal/domain"
)

// AccountingClient legacy stub.
//
// 跟旧 codebase 的字段保持二进制接口 (避免外部 caller 编译失败), 但所有 RPC 方法
// 都返 ErrAccountingNotWired.
type AccountingClient struct {
	BusinessType string // 老字段; 实际不用
	ProductCode  string
	SceneCode    string
	Timeout      int // 老字段; 实际不用
}

// NewAccountingClient — 老签名兼容. 接受任何 gRPC client 参数, 直接忽略.
func NewAccountingClient(_ any) *AccountingClient {
	return &AccountingClient{
		BusinessType: "split_payment",
		ProductCode:  "split_payment",
		SceneCode:    "split_marketplace",
	}
}

// ErrAccountingNotWired — 老 PostMovements / PostSplitAtomic 等都返这个.
// SP-AC-7 已用 AccountingGRPCClient.CreateTransaction 取代.
var ErrAccountingNotWired = errors.New("legacy AccountingClient stub: PostMovements path removed, use AccountingGRPCClient (TransactionService) instead")

// PostMovements legacy 接口, stub 返错.
func (a *AccountingClient) PostMovements(_ context.Context, _ *domain.RunPlan) (string, []string, error) {
	return "", nil, ErrAccountingNotWired
}

// PostSplitAtomic legacy 接口, stub 返错.
func (a *AccountingClient) PostSplitAtomic(_ context.Context, _ *domain.Plan) (string, []string, error) {
	return "", nil, ErrAccountingNotWired
}

// ReverseSplit legacy 接口, stub 返错.
func (a *AccountingClient) ReverseSplit(_ context.Context, _ *domain.LegacyReversal, _ *domain.Plan) (string, error) {
	return "", ErrAccountingNotWired
}

// GetBalance legacy 接口, stub 返 0.
func (a *AccountingClient) GetBalance(_ context.Context, _, _ string) (int64, error) {
	return 0, ErrAccountingNotWired
}

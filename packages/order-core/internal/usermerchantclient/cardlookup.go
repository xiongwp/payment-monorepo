// Package usermerchantclient 给 order-core 用的 user-merchant-core 客户端补充：
// 在 Confirm 路径根据 (user_id, user_card_id) 查 stored_token + masked_pan + network。
//
// 注意：返回的 stored_token 仅在 order-core 内 Confirm 函数 stack 内出现，
// 拿到后立即用来调 card-center.CreatePaymentToken，绝不外发 / 不存。
package usermerchantclient

import (
	"context"
	"errors"
	"time"
)

// CardLookup 接口：仅为 order-core 内 Confirm 路径定义。生产实现走 mTLS gRPC
// 调 user-merchant-core 的内部 RPC（user-merchant-core 暴露专门的 internal-only
// 服务，CN 白名单仅含 order-core）。
//
// 当前 stub：等 user-merchant-core 暴露对应 RPC 后接通。
type CardLookup interface {
	GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (storedToken, maskedPAN, network string, err error)
}

// stubLookup 占位
type stubLookup struct{}

// NewStub 给 dev / 单测用
func NewStub() CardLookup { return &stubLookup{} }

func (stubLookup) GetStoredTokenForPayment(ctx context.Context, userID, userCardID int64) (string, string, string, error) {
	_ = ctx
	if userID == 0 || userCardID == 0 {
		return "", "", "", errors.New("user_id / user_card_id required")
	}
	return "", "", "", errors.New("usermerchantclient: TODO wire real RPC to user-merchant-core")
}

// 构造一个 deadline ctx helper（暂未用，给真实 client impl 时用）
func withTimeout(ctx context.Context, t time.Duration) (context.Context, context.CancelFunc) {
	if t <= 0 {
		t = 3 * time.Second
	}
	return context.WithTimeout(ctx, t)
}

var _ = withTimeout // 暂保留

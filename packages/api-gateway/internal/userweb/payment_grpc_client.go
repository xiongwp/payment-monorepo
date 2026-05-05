// payment_grpc_client.go: real PaymentServiceClient implementation backed by
// order-core.PaymentIntentService gRPC.
//
// ⚠️ COMPILE PREREQUISITE:
//   1. cd packages/order-core && make proto
//      (regenerate order.pb.go with new user_id / user_card_id fields)
//   2. then *CreatePaymentIntentRequest 引用的 GetUserId()/GetUserCardId() 才编译过
//
// Wiring (in main.go after make proto):
//
//	conn := dialOrderCore(...)                          // 新加的 order-core mTLS conn
//	paymentClient := userweb.NewGRPCPaymentClient(conn)
//	ch := userweb.NewCardHandler(uw, cardClient, paymentClient)
//
// 流程：本 client 的 CreateAndConfirmCardPayment 把 cards.go 的 CreatePIReq 翻译成
// order-core.PaymentIntentService.Create + Confirm 两次 RPC（或 Confirm 单次：取决于
// confirmation_method=automatic）。
//
// 调用 Confirm 时不显式带 charge 字段；order-core 内部 service.CardPaymentService.PreparePaymentToken
// 会用 (user_id, user_card_id) → user-merchant-core 拿 stored_token → card-center 派生
// payment_token，最后通过 channel.PaymentRequest.PaymentMethodRef 透传给 payment-channel
// 的 card adapter。
package userweb

import (
	"context"
	"fmt"

	orderv1 "github.com/xiongwp/order-core/api/proto/order/v1"
	"google.golang.org/grpc"
)

// grpcPaymentClient 通过 mTLS gRPC 调 order-core.PaymentIntentService。
type grpcPaymentClient struct {
	pi orderv1.PaymentIntentServiceClient
}

// NewGRPCPaymentClient 装配真实 client。
func NewGRPCPaymentClient(conn *grpc.ClientConn) PaymentServiceClient {
	return &grpcPaymentClient{pi: orderv1.NewPaymentIntentServiceClient(conn)}
}

// CreateAndConfirmCardPayment 单次往返：Create + Confirm 内联（confirmation_method=automatic）。
//
// 顺序：
//   1. PI.Create(amount, currency, user_id, user_card_id, idempotency_key) → 返 pi_id
//   2. PI.Confirm(pi_id, payment_method="card") → order-core 内部触发卡支付链路
//   3. 返结果给 cards.go handler
//
// idempotency_key 由 cards.go 派生（user + card + amount + currency + 5min 时间窗），
// 重复提交 → order-core uk_idem 兜底返同一 PI。
func (g *grpcPaymentClient) CreateAndConfirmCardPayment(ctx context.Context, in *CreatePIReq) (*CreatePIResp, error) {
	if in == nil || in.UserID == 0 || in.UserCardID == 0 {
		return nil, fmt.Errorf("create_pi: user_id / user_card_id required")
	}
	createResp, err := g.pi.Create(ctx, &orderv1.CreatePaymentIntentRequest{
		Amount:              in.Amount,
		Currency:            in.Currency,
		Description:         in.Description,
		IdempotencyKey:      in.IdempotencyKey,
		UserId:              in.UserID,
		UserCardId:          in.UserCardID,
		PaymentMethodTypes:  []string{"card"},
		ConfirmationMethod:  orderv1.ConfirmationMethod_CONFIRMATION_METHOD_AUTOMATIC,
		CaptureMethod:       orderv1.CaptureMethod_CAPTURE_METHOD_AUTOMATIC,
	})
	if err != nil {
		return nil, fmt.Errorf("PI.Create: %w", err)
	}
	pi := createResp.GetPaymentIntent()
	if pi == nil || pi.GetId() == "" {
		return nil, fmt.Errorf("PI.Create returned empty PI")
	}
	confirmResp, err := g.pi.Confirm(ctx, &orderv1.ConfirmPaymentIntentRequest{
		Id:            pi.GetId(),
		PaymentMethod: "card",
	})
	if err != nil {
		return nil, fmt.Errorf("PI.Confirm: %w", err)
	}
	out := &CreatePIResp{
		PIID:   pi.GetId(),
		Status: pi.GetStatus().String(),
	}
	if confirmed := confirmResp.GetPaymentIntent(); confirmed != nil {
		out.Status = confirmed.GetStatus().String()
	}
	if ch := confirmResp.GetCharge(); ch != nil {
		out.DeclineCode = ch.GetFailureCode()
		out.DeclineReason = ch.GetFailureMessage()
	}
	return out, nil
}

// GetPI 拉 PI 状态（pay_result 页用）
func (g *grpcPaymentClient) GetPI(ctx context.Context, piID string) (*PIInfo, error) {
	resp, err := g.pi.Retrieve(ctx, &orderv1.RetrievePaymentIntentRequest{Id: piID})
	if err != nil {
		return nil, err
	}
	pi := resp.GetPaymentIntent()
	if pi == nil {
		return nil, fmt.Errorf("pi not found: %s", piID)
	}
	return &PIInfo{
		ID:       pi.GetId(),
		Amount:   pi.GetAmount(),
		Currency: pi.GetCurrency(),
		Status:   pi.GetStatus().String(),
		// MaskedPAN / Network 在 PI 上没有；payment_token_used 表里有，
		// 走 card-center 的 forensic 查询接口（task: 暴露 GetPaymentForensic RPC）。
	}, nil
}

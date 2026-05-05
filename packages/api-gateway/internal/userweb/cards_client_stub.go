// cards_client_stub.go: 临时 CardServiceClient / PaymentServiceClient 占位实现。
//
// 设计意图：
//
//	cards.go 把 CardServiceClient / PaymentServiceClient 定义成接口，
//	main.go 应注入真实 mTLS gRPC 客户端：
//	  - CardServiceClient   → user-merchant-core.UserCardService gRPC stub
//	  - PaymentServiceClient → order-core.PaymentIntentService（带 user_card_id 字段的扩展）
//
//	真实 client 需要先 `make -C packages/user-merchant-core proto` 生成 user_card.pb.go +
//	user_card_grpc.pb.go，以及在 order-core 加 user_card_id 字段后 `make -C packages/order-core proto`。
//
//	在那之前，本文件提供 stub，让：
//	  1. api-gateway 编译通过
//	  2. /cards、/cards/new、/pay、/pay/result 页面正常渲染
//	  3. 任何写操作返回明确错误信息（不静默失败 / 不假装成功）
//
// 替换路径（接通真实服务后）：
//
//	main.go 里删除 NewStubCardClient / NewStubPaymentClient 调用，
//	换成 NewGRPCCardClient(userMerchantConn) / NewGRPCPaymentClient(orderCoreConn)。
package userweb

import (
	"context"
	"errors"
)

// errCardServiceNotWired card 服务未接通错误（dev 友好）。
var errCardServiceNotWired = errors.New(
	"UserCardService 尚未接通：请先在 user-merchant-core 暴露 UserCardService gRPC，" +
		"再在 api-gateway main.go 注入真实 client（替换 NewStubCardClient）")

// errPaymentServiceNotWired 支付服务未接通错误。
var errPaymentServiceNotWired = errors.New(
	"PaymentService(card) 尚未接通：请先在 order-core PI Confirm 加 user_card_id 字段，" +
		"再在 api-gateway main.go 注入真实 client（替换 NewStubPaymentClient）")

// stubCardClient 让 /cards GET 渲染空列表，所有写操作返回错误。
type stubCardClient struct{}

// NewStubCardClient 给 main.go 用：cards.go CardServiceClient 占位
func NewStubCardClient() CardServiceClient { return &stubCardClient{} }

func (stubCardClient) AttachCard(ctx context.Context, in *AttachCardReq) (*AttachCardResp, error) {
	_ = ctx
	_ = in
	return nil, errCardServiceNotWired
}

func (stubCardClient) ListCards(ctx context.Context, userID int64) ([]CardInfo, error) {
	_ = ctx
	_ = userID
	// 返回空列表 + 不报错：让 /cards 页面能正常渲染（用户看到 "尚未绑卡"）
	return nil, nil
}

func (stubCardClient) DeleteCard(ctx context.Context, userID, userCardID int64) error {
	_ = ctx
	_ = userID
	_ = userCardID
	return errCardServiceNotWired
}

func (stubCardClient) SetDefaultCard(ctx context.Context, userID, userCardID int64) error {
	_ = ctx
	_ = userID
	_ = userCardID
	return errCardServiceNotWired
}

// stubPaymentClient 让 /pay 页面渲染（卡列表为空 → 友好提示），提交支付返回错误。
type stubPaymentClient struct{}

// NewStubPaymentClient 给 main.go 用
func NewStubPaymentClient() PaymentServiceClient { return &stubPaymentClient{} }

func (stubPaymentClient) CreateAndConfirmCardPayment(ctx context.Context, in *CreatePIReq) (*CreatePIResp, error) {
	_ = ctx
	_ = in
	return nil, errPaymentServiceNotWired
}

func (stubPaymentClient) GetPI(ctx context.Context, piID string) (*PIInfo, error) {
	_ = ctx
	_ = piID
	return nil, errPaymentServiceNotWired
}

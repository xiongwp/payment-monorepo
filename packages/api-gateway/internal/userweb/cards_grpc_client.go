// cards_grpc_client.go: real CardServiceClient implementation backed by
// user-merchant-core.UserCardService gRPC stub.
//
// ⚠️ COMPILE PREREQUISITE:
//   1. cd packages/user-merchant-core && make proto
//      (regenerates user_card.pb.go + user_card_grpc.pb.go)
//   2. then this file's references to usercardservice.Client /
//      *AttachCardRequest / *CardInfo will resolve.
//
// Wiring (in main.go after make proto):
//
//	conn := dialUserMerchant(...)                   // existing api-gateway → UMC mTLS conn
//	cardClient := userweb.NewGRPCCardClient(conn)
//	ch := userweb.NewCardHandler(uw, cardClient, paymentClient)
//
// 安全纪律：本 client 把 PCI 严格的 cards.go AttachCardReq 字段（**无 PAN/CVV**）
// 1:1 映射到 proto AttachCardRequest。任何字段加法应同步检查是否引入 PAN。
package userweb

import (
	"context"

	usermerchantv1 "reconcile-system/packages/user-merchant-core/kitex_gen/usermerchant/v1"
	"google.golang.org/grpc"
)

// grpcCardClient 通过 mTLS gRPC 调 user-merchant-core.UserCardService。
type grpcCardClient struct {
	uc usercardservice.Client
}

// NewGRPCCardClient 装配真实 client。conn 必须连到 user-merchant-core 的 listener。
//
// 调用方负责 conn 生命周期（fx OnStop 关连接）；本 client 只持引用。
func NewGRPCCardClient(conn *grpc.ClientConn) CardServiceClient {
	return &grpcCardClient{uc: usermerchantv1.NewUserCardServiceClient(conn)}
}

// AttachCard 把 stored_token + 元数据持久化到 user_card 表。
//
// 请求里**绝对不含 PAN/CVV**：cards.go AttachCardReq 已在源头剥离过 PAN，
// 本函数 1:1 映射到 proto。
func (g *grpcCardClient) AttachCard(ctx context.Context, in *AttachCardReq) (*AttachCardResp, error) {
	resp, err := g.uc.AttachCard(ctx, &usermerchantv1.AttachCardRequest{
		UserId:      in.UserID,
		StoredToken: in.StoredToken,
		MaskedPan:   in.MaskedPAN,
		Network:     in.Network,
		ExpMonth:    int32(in.ExpMonth),
		ExpYear:     int32(in.ExpYear),
		HolderName:  in.HolderName,
		SetDefault:  in.SetDefault,
		TraceId:     in.TraceID,
	})
	if err != nil {
		return nil, err
	}
	return &AttachCardResp{
		UserCardID: resp.GetUserCardId(),
		MaskedPAN:  resp.GetMaskedPan(),
		Network:    resp.GetNetwork(),
	}, nil
}

func (g *grpcCardClient) ListCards(ctx context.Context, userID int64) ([]CardInfo, error) {
	resp, err := g.uc.ListCards(ctx, &usermerchantv1.ListCardsRequest{UserId: userID})
	if err != nil {
		return nil, err
	}
	pb := resp.GetCards()
	out := make([]CardInfo, 0, len(pb))
	for _, c := range pb {
		if c == nil {
			continue
		}
		// 仅展示 active；user-merchant-core ListCards 默认就是 active 过滤
		out = append(out, CardInfo{
			ID:         c.GetId(),
			MaskedPAN:  c.GetMaskedPan(),
			Network:    c.GetNetwork(),
			ExpMonth:   int(c.GetExpMonth()),
			ExpYear:    int(c.GetExpYear()),
			HolderName: c.GetHolderName(),
			IsDefault:  c.GetIsDefault(),
		})
	}
	return out, nil
}

func (g *grpcCardClient) DeleteCard(ctx context.Context, userID, userCardID int64) error {
	_, err := g.uc.DeleteCard(ctx, &usermerchantv1.DeleteCardRequest{
		UserId: userID, UserCardId: userCardID,
	})
	return err
}

func (g *grpcCardClient) SetDefaultCard(ctx context.Context, userID, userCardID int64) error {
	_, err := g.uc.SetDefaultCard(ctx, &usermerchantv1.SetDefaultCardRequest{
		UserId: userID, UserCardId: userCardID,
	})
	return err
}

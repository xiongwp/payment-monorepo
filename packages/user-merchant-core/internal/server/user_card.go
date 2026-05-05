// user_card.go: gRPC adapter for service.UserCardService.
//
// ⚠️ COMPILE PREREQUISITE: run `make proto` first to regenerate user_card.pb.go +
// user_card_grpc.pb.go from api/proto/usermerchant/v1/user_card.proto. Without
// that, references to usermerchantv1.UserCardServiceServer / *AttachCardRequest /
// *CardInfo / etc. will fail to compile.
//
// Wiring:
//
//	浏览器 (HTTPS, PAN) → card-center.tokenize → 浏览器拿 stored_token
//	浏览器 (HTTPS, stored_token) → api-gateway /cards/attach
//	api-gateway → user-merchant-core.UserCardService.AttachCard (本文件)
//	  → service.UserCardService.AttachCard 写 user_card 表（无 PAN）
//
// PCI 严格：本文件 / handler / service / repo 全程不接受 PAN 字段。
package server

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	usermerchantv1 "github.com/xiongwp/user-merchant-core/api/proto/usermerchant/v1"
	"github.com/xiongwp/user-merchant-core/internal/domain"
	"github.com/xiongwp/user-merchant-core/internal/service"
)

// errInvalidArg gRPC InvalidArgument helper（本文件 local；errors.go 里 grpcErr 走通用 sentinel 映射）
func errInvalidArg(msg string) error {
	return status.Error(codes.InvalidArgument, msg)
}

// mapDomainErr domain error → gRPC status；委托给 grpcErr（errors.go）
func mapDomainErr(err error) error { return grpcErr(err) }

// UserCardServer adapts *service.UserCardService onto the gRPC UserCardService.
type UserCardServer struct {
	usermerchantv1.UnimplementedUserCardServiceServer
	usermerchantv1.UnimplementedUserCardInternalServiceServer
	svc *service.UserCardService
}

// NewUserCardServer 构造
func NewUserCardServer(svc *service.UserCardService) *UserCardServer {
	return &UserCardServer{svc: svc}
}

// ─── public-facing UserCardService ──────────────────────────────────────────

// AttachCard 把已经在 card-center 拿到的 stored_token 持久化进 user_card 表。
// **接口契约：永远不接受 PAN/CVV**。
func (s *UserCardServer) AttachCard(ctx context.Context, req *usermerchantv1.AttachCardRequest) (*usermerchantv1.AttachCardResponse, error) {
	if req == nil || req.GetUserId() == 0 || req.GetStoredToken() == "" {
		return nil, errInvalidArg("user_id / stored_token required")
	}
	out, err := s.svc.AttachCard(ctx, &service.AttachCardInput{
		UserID:      req.GetUserId(),
		StoredToken: req.GetStoredToken(),
		MaskedPAN:   req.GetMaskedPan(),
		Network:     req.GetNetwork(),
		ExpMonth:    int(req.GetExpMonth()),
		ExpYear:     int(req.GetExpYear()),
		HolderName:  req.GetHolderName(),
		SetDefault:  req.GetSetDefault(),
		TraceID:     req.GetTraceId(),
	})
	if err != nil {
		return nil, mapDomainErr(err)
	}
	return &usermerchantv1.AttachCardResponse{
		UserCardId: out.UserCardID,
		MaskedPan:  out.MaskedPAN,
		Network:    out.Network,
	}, nil
}

// ListCards 列出用户的 active 卡。脱敏字段 only。
func (s *UserCardServer) ListCards(ctx context.Context, req *usermerchantv1.ListCardsRequest) (*usermerchantv1.ListCardsResponse, error) {
	if req == nil || req.GetUserId() == 0 {
		return nil, errInvalidArg("user_id required")
	}
	cards, err := s.svc.ListCards(ctx, req.GetUserId())
	if err != nil {
		return nil, mapDomainErr(err)
	}
	out := make([]*usermerchantv1.CardInfo, 0, len(cards))
	for _, c := range cards {
		out = append(out, pbCardInfo(c))
	}
	return &usermerchantv1.ListCardsResponse{Cards: out}, nil
}

func (s *UserCardServer) DeleteCard(ctx context.Context, req *usermerchantv1.DeleteCardRequest) (*usermerchantv1.DeleteCardResponse, error) {
	if req == nil || req.GetUserId() == 0 || req.GetUserCardId() == 0 {
		return nil, errInvalidArg("user_id / user_card_id required")
	}
	if err := s.svc.DeleteCard(ctx, req.GetUserId(), req.GetUserCardId(), req.GetTraceId()); err != nil {
		return nil, mapDomainErr(err)
	}
	return &usermerchantv1.DeleteCardResponse{Ok: true}, nil
}

func (s *UserCardServer) SetDefaultCard(ctx context.Context, req *usermerchantv1.SetDefaultCardRequest) (*usermerchantv1.SetDefaultCardResponse, error) {
	if req == nil || req.GetUserId() == 0 || req.GetUserCardId() == 0 {
		return nil, errInvalidArg("user_id / user_card_id required")
	}
	if err := s.svc.SetDefault(ctx, req.GetUserId(), req.GetUserCardId()); err != nil {
		return nil, mapDomainErr(err)
	}
	return &usermerchantv1.SetDefaultCardResponse{Ok: true}, nil
}

// ─── internal-only UserCardInternalService ─────────────────────────────────
//
// 仅暴露在 internal listener；clientCN 白名单 = order-core。

func (s *UserCardServer) GetStoredTokenForPayment(ctx context.Context, req *usermerchantv1.GetStoredTokenRequest) (*usermerchantv1.GetStoredTokenResponse, error) {
	if req == nil || req.GetUserId() == 0 || req.GetUserCardId() == 0 {
		return nil, errInvalidArg("user_id / user_card_id required")
	}
	tok, masked, network, err := s.svc.GetStoredTokenForPayment(ctx, req.GetUserId(), req.GetUserCardId())
	if err != nil {
		return nil, mapDomainErr(err)
	}
	return &usermerchantv1.GetStoredTokenResponse{
		StoredToken: tok,
		MaskedPan:   masked,
		Network:     network,
	}, nil
}

// ─── helpers ───────────────────────────────────────────────────────────────

func pbCardInfo(c *domain.UserCard) *usermerchantv1.CardInfo {
	if c == nil {
		return nil
	}
	return &usermerchantv1.CardInfo{
		Id:         c.ID,
		MaskedPan:  c.MaskedPAN,
		Network:    c.Network,
		ExpMonth:   int32(c.ExpMonth),
		ExpYear:    int32(c.ExpYear),
		HolderName: c.HolderName,
		IsDefault:  c.IsDefault,
		Status:     string(c.Status),
	}
}


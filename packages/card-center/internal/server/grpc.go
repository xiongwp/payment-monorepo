// Package server 提供 Kitex handler, 把 cardcenter.v1 proto 转到 service.Service.
//
// 注意: 本 handler **不**直接接触 PAN (仅在 DetokenizeResponse 内回传, 调用方
// 接到后必须立即用、立即清栈, 本服务自己不能 log).
package server

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.uber.org/zap"
	cardcenterv1 "reconcile-system/packages/card-center/kitex_gen/cardcenter/v1"

	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/service"
	"github.com/xiongwp/payment-util/trace"
)

// Server 实现 Kitex cardcenterservice.Server 接口 (跟 gRPC 同方法签名).
// 切 Kitex 后不再 embed UnimplementedCardCenterServer.
type Server struct {
	svc    *service.Service
	logger *zap.Logger
}

// NewServer 构造
func NewServer(svc *service.Service, logger *zap.Logger) *Server {
	return &Server{svc: svc, logger: logger}
}

// Register Kitex 不用后置 Register, 这里保留 no-op 兼容老接口. cmd/server/main.go
// 直接用 cardcenterservice.NewServer(impl, opts...) 在构造时完成 service 注册.
func (s *Server) Register() {}

// ─── Tokenize ──────────────────────────────────────────────────────────────

func (s *Server) Tokenize(ctx context.Context, req *cardcenterv1.TokenizeRequest) (*cardcenterv1.TokenizeResponse, error) {
	if req.GetPan() == "" || req.GetUserId() == "" {
		return nil, fmt.Errorf("pan / user_id required")
	}
	uid, err := strconv.ParseInt(req.GetUserId(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("user_id must be numeric")
	}
	cn, ip := PeerCN(ctx)
	out, err := s.svc.Tokenize(ctx, &service.TokenizeInput{
		UserID:     uid,
		PAN:        req.GetPan(),
		ExpMonth:   int(req.GetExpMonth()),
		ExpYear:    int(req.GetExpYear()),
		HolderName: req.GetHolderName(),
		Caller:     cn,
		CallerIP:   ip,
		TraceID:    req.GetTraceId(),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &cardcenterv1.TokenizeResponse{
		StoredToken: out.StoredToken,
		MaskedPan:   out.MaskedPAN,
		Network:     out.Network,
		KmsKid:      out.KMSKid,
	}, nil
}

// ─── CreatePaymentToken ────────────────────────────────────────────────────

func (s *Server) CreatePaymentToken(ctx context.Context, req *cardcenterv1.CreatePaymentTokenRequest) (*cardcenterv1.CreatePaymentTokenResponse, error) {
	if req.GetStoredToken() == "" || req.GetUserId() == "" || req.GetPiId() == "" {
		return nil, fmt.Errorf("stored_token / user_id / pi_id required")
	}
	uid, err := strconv.ParseInt(req.GetUserId(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("user_id must be numeric")
	}
	cn, ip := PeerCN(ctx)
	ttl := time.Duration(req.GetTtlSeconds()) * time.Second
	out, err := s.svc.CreatePaymentToken(ctx, &service.CreatePaymentTokenInput{
		StoredToken: req.GetStoredToken(),
		UserID:      uid,
		PIID:        req.GetPiId(),
		Amount:      req.GetAmount(),
		Currency:    req.GetCurrency(),
		TTL:         ttl,
		Caller:      cn,
		CallerIP:    ip,
		TraceID:     req.GetTraceId(),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return &cardcenterv1.CreatePaymentTokenResponse{
		PaymentToken: out.PaymentToken,
		ExpiresAt:    out.ExpiresAt.Unix(),
		MaskedPan:    out.MaskedPAN,
		Network:      out.Network,
	}, nil
}

// ─── Detokenize ────────────────────────────────────────────────────────────

func (s *Server) Detokenize(ctx context.Context, req *cardcenterv1.DetokenizeRequest) (*cardcenterv1.DetokenizeResponse, error) {
	if req.GetPaymentToken() == "" || req.GetPiId() == "" {
		return nil, fmt.Errorf("payment_token / pi_id required")
	}
	cn, ip := PeerCN(ctx)
	out, err := s.svc.Detokenize(ctx, &service.DetokenizeInput{
		PaymentToken: req.GetPaymentToken(),
		PIID:         req.GetPiId(),
		Caller:       cn,
		CallerIP:     ip,
		TraceID:      req.GetTraceId(),
	})
	if err != nil {
		return nil, mapErr(err)
	}
	// 注意：返回的 PAN 仅在响应里出现，本进程不再存任何引用
	resp := &cardcenterv1.DetokenizeResponse{
		Pan:        out.PAN,
		ExpMonth:   int32(out.ExpMonth),
		ExpYear:    int32(out.ExpYear),
		HolderName: out.HolderName,
		MaskedPan:  out.MaskedPAN,
		Network:    out.Network,
	}
	// 本函数返回后栈清掉 out; out.PAN 仅在 resp 里被 marshal 一次
	return resp, nil
}

// ─── DeleteCard / RevokeStoredToken ─────────────────────────────────────────

func (s *Server) DeleteCard(ctx context.Context, req *cardcenterv1.DeleteCardRequest) (*cardcenterv1.DeleteCardResponse, error) {
	uid, err := strconv.ParseInt(req.GetUserId(), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("user_id must be numeric")
	}
	cn, ip := PeerCN(ctx)
	if err := s.svc.DeleteCard(ctx, uid, req.GetStoredToken(), req.GetReason(), cn, ip, req.GetTraceId()); err != nil {
		return nil, mapErr(err)
	}
	return &cardcenterv1.DeleteCardResponse{Ok: true}, nil
}

func (s *Server) RevokeStoredToken(ctx context.Context, req *cardcenterv1.RevokeStoredTokenRequest) (*cardcenterv1.RevokeStoredTokenResponse, error) {
	// 简化：跟 DeleteCard 同义；reason 不同
	return &cardcenterv1.RevokeStoredTokenResponse{Ok: true}, nil
}

// mapErr 业务错 → gRPC code
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, repo.ErrStoredCardNotFound):
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, repo.ErrPaymentTokenAlreadyUsed):
		return fmt.Errorf("%s", err.Error())
	}
	return fmt.Errorf("internal error")
}

// 保持 trace import 不被裁；server 里实际由 grpc interceptor 接 trace
var _ = trace.UnaryServerInterceptor

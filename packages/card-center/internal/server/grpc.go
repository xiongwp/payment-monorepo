// Package server 提供 Kitex handler, 把 cardcenter.v1 proto 转到 service.Service.
//
// 注意: 本 handler **不**直接接触 PAN (仅在 DetokenizeResponse 内回传, 调用方
// 接到后必须立即用、立即清栈, 本服务自己不能 log).
package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.uber.org/zap"
	cardcenterv1 "github.com/xiongwp/card-center/kitex_gen/cardcenter/v1"

	"github.com/xiongwp/card-center/internal/repo"
	"github.com/xiongwp/card-center/internal/service"
)

// PeerCN STUB: gRPC mTLS 切 Kitex 后 peer.FromContext / credentials.TLSInfo
// 已废, 本函数返回空 cn + 空 ip — 业务路径(audit / Tokenize 等)用空值表示
// "未识别 caller", 接通 Kitex MTLSClientCNMW 后改回真值.
func PeerCN(_ context.Context) (cn, ip string) { return "", "" }

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
	// MED-FIX-1: 之前 stub 直接返 Ok=true 但什么都没做 — admin 以为 revoke 成功
	// 实际 token 仍 active, 是安全漏洞 (compromised token 没被 invalidate).
	//
	// 根因: proto 只有 stored_token + reason, 没 user_id; 而 stored_card 表按 user_id
	// shard, repo.SoftDelete 必须知道 user_id 才能定位 shard. 解法二选一:
	//   1. proto 加 user_id 字段 (推荐, 长期). caller 已经持有 user_id 时优先调 DeleteCard.
	//   2. 加 cross-shard 扫描的 RevokeByTokenHash service 方法 (慢, 适合 ops 应急工具).
	//
	// 在 proto 升级前, 拒绝 silent ok — 让 caller 看到 Unimplemented 显式失败,
	// 避免合规审计时拿一堆假成功的 audit log.
	if s.logger != nil {
		s.logger.Warn("RevokeStoredToken called but unimplemented (proto missing user_id); use DeleteCard with user_id instead",
			zap.String("trace_id", req.GetTraceId()),
			zap.String("reason", req.GetReason()))
	}
	return &cardcenterv1.RevokeStoredTokenResponse{Ok: false},
		fmt.Errorf("RevokeStoredToken unimplemented: proto lacks user_id required for sharded lookup; use DeleteCard (user_id + stored_token) instead")
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


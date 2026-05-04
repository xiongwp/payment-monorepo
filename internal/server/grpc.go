// Package server 实现 paymentcorev1.PaymentCoreServiceServer —— 即 order-core
// 视角下的 PaymentChannel 远端。
package server

import (
	"context"
	"fmt"
	"net"

	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	"github.com/xiongwp/payment-core/internal/channel"
	"github.com/xiongwp/payment-core/internal/service"
)

type Server struct {
	paymentcorev1.UnimplementedPaymentCoreServiceServer

	svc    *service.PaymentService
	whSvc  *service.WebhookService
	auth   map[string]string
	rps    float64
	burst  int
	logger *zap.Logger
}

type Deps struct {
	PaymentSvc   *service.PaymentService
	WebhookSvc   *service.WebhookService
	AuthTokens   map[string]string
	RateLimitRPS float64
	RateBurst    int
	Logger       *zap.Logger
}

func NewServer(d Deps) *Server {
	return &Server{
		svc:    d.PaymentSvc,
		whSvc:  d.WebhookSvc,
		auth:   d.AuthTokens,
		rps:    d.RateLimitRPS,
		burst:  d.RateBurst,
		logger: d.Logger,
	}
}

func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	srv := grpc.NewServer(grpc.ChainUnaryInterceptor(
		RecoverInterceptor(s.logger),
		trace.UnaryServerInterceptor(s.logger),
		LoggingInterceptor(s.logger),
		MetricsInterceptor(),
		RateLimitInterceptor(s.rps, s.burst),
		AuthInterceptor(s.auth, s.logger),
	))
	paymentcorev1.RegisterPaymentCoreServiceServer(srv, s)
	s.logger.Info("payment-core grpc listening", zap.Int("port", port))
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	return srv.Serve(lis)
}

// ─── RPC handlers ────────────────────────────────────────────────

func (s *Server) Charge(ctx context.Context, req *paymentcorev1.ChargeRequest) (*paymentcorev1.ChargeResponse, error) {
	if req.GetPaymentIntentId() == "" {
		return nil, status.Error(codes.InvalidArgument, "payment_intent_id required")
	}
	out, err := s.svc.Charge(ctx, &channel.PaymentRequest{
		PaymentIntentID:  req.GetPaymentIntentId(),
		ChargeID:         req.GetChargeId(),
		Amount:           req.GetAmount(),
		Currency:         req.GetCurrency(),
		Country:          req.GetCountry(),
		PaymentMethod:    req.GetPaymentMethod(),
		PaymentMethodRef: req.GetPaymentMethodRef(),
		CustomerID:       req.GetCustomerId(),
		CaptureMethod:    req.GetCaptureMethod(),
		ReturnURL:        req.GetReturnUrl(),
		NotifyURL:        req.GetNotifyUrl(),
		Description:      req.GetDescription(),
		Metadata:         req.GetMetadata(),
		Extra:            req.GetExtra(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return paymentResponseToProto(out), nil
}

func (s *Server) Capture(ctx context.Context, req *paymentcorev1.CaptureRequest) (*paymentcorev1.OpResponse, error) {
	out, err := s.svc.Capture(ctx, &channel.CaptureRequest{
		PaymentIntentID: req.GetPaymentIntentId(),
		ChargeID:        req.GetChargeId(),
		ExternalRefNo:   req.GetExternalRefNo(),
		Amount:          req.GetAmount(),
		Extra:           req.GetExtra(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &paymentcorev1.OpResponse{
		ResultType:     string(out.ResultType),
		ExternalRefNo:  out.ExternalRefNo,
		FailureCode:    out.FailureCode,
		FailureMessage: out.FailureMessage,
		RawResponse:    out.RawResponse,
	}, nil
}

func (s *Server) Void(ctx context.Context, req *paymentcorev1.VoidRequest) (*paymentcorev1.OpResponse, error) {
	out, err := s.svc.Void(ctx, &channel.VoidRequest{
		PaymentIntentID: req.GetPaymentIntentId(),
		ChargeID:        req.GetChargeId(),
		ExternalRefNo:   req.GetExternalRefNo(),
		Reason:          req.GetReason(),
		Extra:           req.GetExtra(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &paymentcorev1.OpResponse{
		ResultType:     string(out.ResultType),
		ExternalRefNo:  out.ExternalRefNo,
		FailureCode:    out.FailureCode,
		FailureMessage: out.FailureMessage,
		RawResponse:    out.RawResponse,
	}, nil
}

func (s *Server) Refund(ctx context.Context, req *paymentcorev1.RefundRequest) (*paymentcorev1.OpResponse, error) {
	out, err := s.svc.Refund(ctx, &channel.RefundChannelRequest{
		PaymentIntentID: req.GetPaymentIntentId(),
		ChargeID:        req.GetChargeId(),
		RefundID:        req.GetRefundId(),
		ExternalRefNo:   req.GetExternalRefNo(),
		Amount:          req.GetAmount(),
		Currency:        req.GetCurrency(),
		Reason:          req.GetReason(),
		Extra:           req.GetExtra(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &paymentcorev1.OpResponse{
		ResultType:     string(out.ResultType),
		ExternalRefNo:  out.ExternalRefundRefNo,
		FailureCode:    out.FailureCode,
		FailureMessage: out.FailureMessage,
		RawResponse:    out.RawResponse,
	}, nil
}

func (s *Server) Query(ctx context.Context, req *paymentcorev1.QueryRequest) (*paymentcorev1.QueryResponse, error) {
	out, err := s.svc.Query(ctx, &channel.QueryRequest{
		PaymentIntentID: req.GetPaymentIntentId(),
		ChargeID:        req.GetChargeId(),
		ExternalRefNo:   req.GetExternalRefNo(),
		Extra:           req.GetExtra(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &paymentcorev1.QueryResponse{
		ResultType:     string(out.ResultType),
		ExternalRefNo:  out.ExternalRefNo,
		AmountCaptured: out.AmountCaptured,
		AmountRefunded: out.AmountRefunded,
		FailureCode:    out.FailureCode,
		FailureMessage: out.FailureMessage,
		RawResponse:    out.RawResponse,
	}, nil
}

func (s *Server) ParseWebhook(ctx context.Context, req *paymentcorev1.ParseWebhookRequest) (*paymentcorev1.WebhookEvent, error) {
	evt, err := s.whSvc.Parse(ctx, req.GetAdapter(), req.GetHeaders(), req.GetBody())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &paymentcorev1.WebhookEvent{
		EventId:         evt.EventID,
		EventType:       evt.EventType,
		PaymentIntentId: evt.PaymentIntentID,
		ChargeId:        evt.ChargeID,
		RefundId:        evt.RefundID,
		ExternalRefNo:   evt.ExternalRefNo,
		Amount:          evt.Amount,
		Timestamp:       evt.Timestamp.Unix(),
		RawPayload:      evt.RawPayload,
	}, nil
}

// ─── helpers ────────────────────────────────────────────────────

func paymentResponseToProto(r *channel.PaymentResponse) *paymentcorev1.ChargeResponse {
	out := &paymentcorev1.ChargeResponse{
		ResultType:       string(r.ResultType),
		ExternalRefNo:    r.ExternalRefNo,
		AuthCode:         r.AuthCode,
		AmountAuthorized: r.AmountAuthorized,
		AmountCaptured:   r.AmountCaptured,
		FailureCode:      r.FailureCode,
		FailureMessage:   r.FailureMessage,
		RiskLevel:        r.RiskLevel,
		RiskScore:        int32(r.RiskScore),
		RawResponse:      r.RawResponse,
	}
	if a := r.RequiredAction; a != nil {
		out.RequiredAction = &paymentcorev1.RequiredAction{
			Type:         string(a.Type),
			ExpiresAt:    a.ExpiresAt.Unix(),
			ChallengeRef: a.ChallengeRef,
			Details:      a.Details,
		}
	}
	return out
}

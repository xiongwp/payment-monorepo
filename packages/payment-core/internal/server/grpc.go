// Package server 实现 paymentcorev1.PaymentCoreServiceServer —— 即 order-core
// 视角下的 PaymentChannel 远端。
package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	paymentcorev1 "github.com/xiongwp/payment-core/api/proto/paymentcore/v1"
	"github.com/xiongwp/payment-core/internal/channel"
	"github.com/xiongwp/payment-core/internal/service"
)

type Server struct {
	paymentcorev1.UnimplementedPaymentCoreServiceServer

	svc                  *service.PaymentService
	whSvc                *service.WebhookService
	auth                 map[string]string
	allowUnauthenticated bool
	rps                  float64
	burst                int
	shutdownTimeout      time.Duration
	logger               *zap.Logger
}

type Deps struct {
	PaymentSvc *service.PaymentService
	WebhookSvc *service.WebhookService
	AuthTokens map[string]string
	// AllowUnauthenticated dev / lab 显式打开匿名（生产应为 false）。
	// AuthTokens 为空 + 本字段 false → 启动期 fail-closed 拒所有请求。
	AllowUnauthenticated bool
	RateLimitRPS         float64
	RateBurst            int
	// ShutdownTimeout SIGTERM 后等待 in-flight RPC 完成的最长时间。
	// 0 = 用默认 25s（与 K8s preStop 30s 留 5s 余量）。
	ShutdownTimeout time.Duration
	Logger          *zap.Logger
}

func NewServer(d Deps) *Server {
	return &Server{
		svc:                  d.PaymentSvc,
		whSvc:                d.WebhookSvc,
		auth:                 d.AuthTokens,
		allowUnauthenticated: d.AllowUnauthenticated,
		rps:                  d.RateLimitRPS,
		burst:                d.RateBurst,
		shutdownTimeout:      d.ShutdownTimeout,
		logger:               d.Logger,
	}
}

func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	srv := grpc.NewServer(
		// EnforcementPolicy 必须放宽：客户端（order-core / 自身 mesh 内 RPC）按
		// keepalive.ClientParameters{Time: 30s, PermitWithoutStream: true} 心跳；
		// gRPC server 默认 MinTime=5min + PermitWithoutStream=false，会以
		// "too_many_pings" GOAWAY 踢连接。这里跟 user-merchant-core / accounting-system
		// 对齐，允许 5s 一次心跳 + 无活动流也可 ping。
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(
			RecoverInterceptor(s.logger),
			trace.UnaryServerInterceptor(s.logger),
			// shadow 紧跟 trace：把 metadata x-shadow 翻进 ctx；payment-core 是无状态路由
			// 层，shadow ctx 仅供日志 + 出站 RPC（channel / kms / risk）透传给下游。
			shadow.UnaryServerInterceptor(),
			LoggingInterceptor(s.logger),
			MetricsInterceptor(),
			RateLimitInterceptor(s.rps, s.burst),
			AuthInterceptor(s.auth, s.allowUnauthenticated, s.logger),
		))
	paymentcorev1.RegisterPaymentCoreServiceServer(srv, s)
	s.logger.Info("payment-core grpc listening", zap.Int("port", port))
	// P1-16 graceful shutdown timeout 兜底：K8s preStop 默认 30s 内必须 drain 完毕，
	// 在那之后 SIGKILL。GracefulStop 不带 timeout 会无限等 in-flight RPC 完成，
	// 一笔慢 charge（payment-channel 卡 60s+）能直接撑到 SIGKILL → 客户端见 RST，
	// 业务侧重试触发幂等冲突。给 25s 上限：超时后 srv.Stop() 强制断连，让客户端
	// 走超时重试比 SIGKILL 优雅。
	shutdownTimeout := s.shutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 25 * time.Second
	}
	go func() {
		<-ctx.Done()
		s.logger.Info("payment-core grpc draining", zap.Duration("timeout", shutdownTimeout))
		stopped := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
			s.logger.Info("payment-core grpc graceful stop complete")
		case <-time.After(shutdownTimeout):
			s.logger.Warn("payment-core grpc graceful stop timed out, forcing", zap.Duration("after", shutdownTimeout))
			srv.Stop()
		}
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

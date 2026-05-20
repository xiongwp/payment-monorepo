// Package server 实现 Kitex paymentcoreservice.Server —— 即 order-core 视角下的
// PaymentChannel 远端. 切 Kitex 后跟 gRPC wire 不互通; 调用方 (order-core /
// payment-admin-web) 已同步切.
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	kitexserver "github.com/cloudwego/kitex/server"
	"github.com/xiongwp/payment-util/kitexutil"
	"go.uber.org/zap"
	paymentcorev1 "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1"
	paymentcoreservice "github.com/xiongwp/payment-core/kitex_gen/paymentcore/v1/paymentcoreservice"

	"github.com/xiongwp/payment-core/internal/channel"
	"github.com/xiongwp/payment-core/internal/service"
)

// Server 实现 Kitex paymentcoreservice.Server 接口 (跟 gRPC 同方法签名).
type Server struct {
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
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	// TODO 接 kitexutil MW (Recover / Trace / Shadow / Logging / Metrics / RateLimit / Auth)
	// — 等 kitexutil port 完成后接 server.WithMiddleware(...).
	advHost := os.Getenv("ADVERTISE_HOST")
	if advHost == "" {
		advHost = "payment-core"
	}
	srvOpts := []kitexserver.Option{kitexserver.WithServiceAddr(addr)}
	srvOpts = append(srvOpts, kitexutil.DefaultServerOptions("payment-core", fmt.Sprintf("%s:%d", advHost, port))...)
	srv := paymentcoreservice.NewServer(s, srvOpts...)
	s.logger.Info("payment-core Kitex listening", zap.Int("port", port))

	// P1-16 graceful shutdown timeout 兜底: K8s preStop 默认 30s 内必须 drain 完毕,
	// 之后 SIGKILL. Kitex srv.Stop() 是 graceful 等 in-flight RPC; 用 select 限上限.
	shutdownTimeout := s.shutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = 25 * time.Second
	}
	go func() {
		<-ctx.Done()
		s.logger.Info("payment-core Kitex draining", zap.Duration("timeout", shutdownTimeout))
		stopped := make(chan struct{})
		go func() {
			_ = srv.Stop()
			close(stopped)
		}()
		select {
		case <-stopped:
			s.logger.Info("payment-core Kitex graceful stop complete")
		case <-time.After(shutdownTimeout):
			s.logger.Warn("payment-core Kitex graceful stop timed out",
				zap.Duration("after", shutdownTimeout))
		}
	}()
	return srv.Run()
}

// ─── RPC handlers ────────────────────────────────────────────────

func (s *Server) Charge(ctx context.Context, req *paymentcorev1.ChargeRequest) (*paymentcorev1.ChargeResponse, error) {
	if req.GetPaymentIntentId() == "" {
		return nil, fmt.Errorf("payment_intent_id required")
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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

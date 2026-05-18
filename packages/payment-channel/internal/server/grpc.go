// Package server gRPC adapter：把 channelv1 RPC 映射到 AcquirerService。
//
// 本文件依赖 `make proto` 生成的 api/proto/channel/v1/channel.pb.go +
// channel_grpc.pb.go 产物；生成前 `go build` 会报 import 找不到，属正常现象。
package server

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/xiongwp/payment-util/piiredact" // ROI-2b: PII-safe access log
	"github.com/xiongwp/payment-util/shadow"
	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	channelv1 "github.com/xiongwp/payment-channel/api/proto/channel/v1"
	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/service"
)

// Server 同时实现 channelv1.AcquirerServiceServer 与 HTTP webhook listener。
type Server struct {
	channelv1.UnimplementedAcquirerServiceServer

	svc *service.AcquirerService

	authTokens           map[string]string
	allowUnauthenticated bool
	// rateLimiter 持有引用以支持 SetRateLimit 热更新（config-center OnChange 调）。
	// nil = 不限流（dev / 测试）。
	rateLimiter *rate.Limiter
	logger      *zap.Logger
}

type Deps struct {
	AcquirerSvc          *service.AcquirerService
	AuthTokens           map[string]string
	AllowUnauthenticated bool // dev / lab 显式打开匿名；生产 false
	RateLimitRPS         float64
	RateBurst            int
	Logger               *zap.Logger
}

func NewServer(d Deps) *Server {
	var lim *rate.Limiter
	if d.RateLimitRPS > 0 {
		burst := d.RateBurst
		if burst <= 0 {
			burst = int(d.RateLimitRPS)
			if burst < 1 {
				burst = 1
			}
		}
		lim = rate.NewLimiter(rate.Limit(d.RateLimitRPS), burst)
	}
	return &Server{
		svc:                  d.AcquirerSvc,
		authTokens:           d.AuthTokens,
		allowUnauthenticated: d.AllowUnauthenticated,
		rateLimiter:          lim,
		logger:               d.Logger,
	}
}

// SetRateLimit 热更新 rps + burst（config-center OnChange 回调里调）。
// rps <= 0 → 关限流（rateLimiter 设 nil）。
func (s *Server) SetRateLimit(rps float64, burst int) {
	if rps <= 0 {
		s.rateLimiter = nil
		return
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	if s.rateLimiter == nil {
		s.rateLimiter = rate.NewLimiter(rate.Limit(rps), burst)
		return
	}
	s.rateLimiter.SetLimit(rate.Limit(rps))
	s.rateLimiter.SetBurst(burst)
}

// ListenAndServe 启动 gRPC。
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	srv := grpc.NewServer(
		// 跟 user-merchant-core / accounting-system 对齐；放宽 grpc default
		// (MinTime=5min, PermitWithoutStream=false) 防 too_many_pings GOAWAY。
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(
			RecoverInterceptor(s.logger),
			trace.UnaryServerInterceptor(s.logger), // 从 metadata 取 x-trace-id 注入 ctx/logger
			// ROI-2b: PII-safe access log — payload logging 通过 env PAYCHAN_LOG_PAYLOAD=1 开关.
			// payment-channel 直接对接 PSP, request 里 buyer email/phone + card token 都是高 PII,
			// 默认只记 method+code+duration; 排查时打开 PAYCHAN_LOG_PAYLOAD=1 拿 Luhn-脱敏后的 payload.
			piiredact.LoggingInterceptor(s.logger, piiredact.LoggingOptions{
				LogPayload: false, // 默认关; ops 手动改 env 重启可开
				SkipMethods: map[string]struct{}{
					"/grpc.health.v1.Health/Check": {},
				},
			}),
			// shadow 标识翻进 ctx；AcquirerService 5 个方法入口检查 IsShadow 短路放行 —
			// 压测流量绝不真打到外部渠道（GCash / Maya 等），返回 mock 结果。
			shadow.UnaryServerInterceptor(),
			MetricsInterceptor(),
			RateLimitInterceptor(s.rateLimiter),
			AuthInterceptor(s.authTokens, s.allowUnauthenticated, s.logger),
		))
	channelv1.RegisterAcquirerServiceServer(srv, s)
	s.logger.Info("payment-channel grpc listening", zap.Int("port", port))
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	return srv.Serve(lis)
}

// ─── RPC handlers ─────────────────────────────────────────────────────

func (s *Server) Charge(ctx context.Context, req *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	if req.GetAdapter() == "" || req.GetPiId() == "" || req.GetIdempotencyKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "adapter / pi_id / idempotency_key required")
	}
	in := &channel.ChargeRequest{
		PiID:             req.GetPiId(),
		IdempotencyKey:   req.GetIdempotencyKey(),
		Amount:           req.GetAmount(),
		Currency:         req.GetCurrency(),
		Description:      req.GetDescription(),
		ReturnURL:        req.GetReturnUrl(),
		NotifyURL:        req.GetNotifyUrl(),
		CaptureImmediate: req.GetCaptureImmediate(),
		Metadata:         req.GetMetadata(),
	}
	if b := req.GetBuyer(); b != nil {
		in.Buyer = channel.Buyer{
			FirstName: b.GetFirstName(),
			LastName:  b.GetLastName(),
			Email:     b.GetEmail(),
			Phone:     b.GetPhone(),
		}
	}
	out, err := s.svc.Charge(ctx, req.GetAdapter(), in)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return chargeRespToProto(out), nil
}

func (s *Server) Capture(ctx context.Context, req *channelv1.CaptureRequest) (*channelv1.OpResponse, error) {
	out, err := s.svc.Capture(ctx, req.GetAdapter(), &channel.CaptureRequest{
		PiID:           req.GetPiId(),
		ExternalRefNo:  req.GetExternalRefNo(),
		Amount:         req.GetAmount(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return opRespToProto(out), nil
}

func (s *Server) Void(ctx context.Context, req *channelv1.VoidRequest) (*channelv1.OpResponse, error) {
	out, err := s.svc.Void(ctx, req.GetAdapter(), &channel.VoidRequest{
		PiID:           req.GetPiId(),
		ExternalRefNo:  req.GetExternalRefNo(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return opRespToProto(out), nil
}

func (s *Server) Refund(ctx context.Context, req *channelv1.RefundRequest) (*channelv1.OpResponse, error) {
	out, err := s.svc.Refund(ctx, req.GetAdapter(), &channel.RefundRequest{
		PiID:           req.GetPiId(),
		ExternalRefNo:  req.GetExternalRefNo(),
		Amount:         req.GetAmount(),
		Reason:         req.GetReason(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return opRespToProto(out), nil
}

func (s *Server) Query(ctx context.Context, req *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	out, err := s.svc.Query(ctx, req.GetAdapter(), &channel.QueryRequest{
		PiID:          req.GetPiId(),
		ExternalRefNo: req.GetExternalRefNo(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &channelv1.QueryResponse{
		Result:         string(out.Result),
		ExternalRefNo:  out.ExternalRefNo,
		AmountCaptured: out.AmountCaptured,
		AmountRefunded: out.AmountRefunded,
		Raw:            out.Raw,
	}, nil
}

// ─── proto <-> domain helpers ────────────────────────────────────────

func chargeRespToProto(r *channel.ChargeResponse) *channelv1.ChargeResponse {
	if r == nil {
		return &channelv1.ChargeResponse{}
	}
	out := &channelv1.ChargeResponse{
		Result:         string(r.Result),
		ExternalRefNo:  r.ExternalRefNo,
		FailureCode:    r.FailureCode,
		RawFailureCode: r.RawFailureCode,
		FailureMessage: r.FailureMessage,
		Raw:            r.Raw,
	}
	if r.RequiredAction != nil {
		a := r.RequiredAction
		out.RequiredAction = &channelv1.RequiredAction{
			Type:        a.Type,
			ExpiresAt:   a.ExpiresAt.Unix(),
			RedirectUrl: a.RedirectURL,
			Scheme:      a.Scheme,
			ReturnUrl:   a.ReturnURL,
			QrCodeUrl:   a.QRCodeURL,
			QrImageB64:  a.QRImageB64,
			PollMs:      int32(a.PollMs),
			OtpMasked:   a.OTPMasked,
			OtpChannel:  a.OTPChannel,
			OtpLength:   int32(a.OTPLength),
			Extra:       a.Extra,
		}
	}
	return out
}

func opRespToProto(r *channel.OpResponse) *channelv1.OpResponse {
	if r == nil {
		return &channelv1.OpResponse{}
	}
	return &channelv1.OpResponse{
		Result:         string(r.Result),
		ExternalRefNo:  r.ExternalRefNo,
		FailureCode:    r.FailureCode,
		RawFailureCode: r.RawFailureCode,
		FailureMessage: r.FailureMessage,
	}
}

// 静态分析别挑 time 没被用；本包里 context/time 都在别处用了
var _ = time.Second

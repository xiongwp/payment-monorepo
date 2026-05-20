// Package server Kitex adapter: 把 channelv1 RPC 映射到 AcquirerService.
//
// 本文件依赖 `./idl/generate.sh channel` 生成的 kitex_gen/channel/v1/{channel.pb.go,
// acquirerservice/} 产物; 生成前 `go build` 会报 import 找不到, 属正常.
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
	"golang.org/x/time/rate"
	channelv1 "github.com/xiongwp/payment-channel/kitex_gen/channel/v1"
	acquirerservice "github.com/xiongwp/payment-channel/kitex_gen/channel/v1/acquirerservice"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/service"
)

// Server 实现 Kitex acquirerservice.Server 接口 (跟 gRPC 同方法签名).
// 切 Kitex 后不再 embed UnimplementedAcquirerServiceServer.
type Server struct {
	svc *service.AcquirerService

	authTokens           map[string]string
	allowUnauthenticated bool
	// rateLimiter 持有引用以支持 SetRateLimit 热更新 (config-center OnChange 调).
	// nil = 不限流 (dev / 测试).
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

// ListenAndServe 启动 Kitex.
//
// TODO: kitexutil MW 三件套 (Recover / Trace / Shadow / PIIRedact / Metrics /
// RateLimit / Auth) — 等 kitexutil port 完成后接 server.WithMiddleware(...).
// 当前 stub: 只启 Kitex server, MW 链全标 TODO.
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	advHost := os.Getenv("ADVERTISE_HOST")
	if advHost == "" {
		advHost = "payment-channel"
	}
	srvOpts := []kitexserver.Option{kitexserver.WithServiceAddr(addr)}
	srvOpts = append(srvOpts, kitexutil.DefaultServerOptions("payment-channel", fmt.Sprintf("%s:%d", advHost, port))...)
	srv := acquirerservice.NewServer(s, srvOpts...)
	s.logger.Info("payment-channel Kitex listening", zap.Int("port", port))
	go func() {
		<-ctx.Done()
		_ = srv.Stop()
	}()
	return srv.Run()
}

// 防 import 未用 (rate / time / shadow / trace / piiredact 等 MW 接好后还要用):
var _ = time.Second

// ─── RPC handlers ─────────────────────────────────────────────────────

func (s *Server) Charge(ctx context.Context, req *channelv1.ChargeRequest) (*channelv1.ChargeResponse, error) {
	if req.GetAdapter() == "" || req.GetPiId() == "" || req.GetIdempotencyKey() == "" {
		return nil, fmt.Errorf("adapter / pi_id / idempotency_key required")
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
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
		return nil, fmt.Errorf("%s", err.Error())
	}
	return opRespToProto(out), nil
}

func (s *Server) Query(ctx context.Context, req *channelv1.QueryRequest) (*channelv1.QueryResponse, error) {
	out, err := s.svc.Query(ctx, req.GetAdapter(), &channel.QueryRequest{
		PiID:          req.GetPiId(),
		ExternalRefNo: req.GetExternalRefNo(),
	})
	if err != nil {
		return nil, fmt.Errorf("%s", err.Error())
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

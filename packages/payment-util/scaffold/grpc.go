// grpc.go — gRPC server 集成.
//
// scaffold 默认起 fiber HTTP server; 加 gRPC 用 Opts.GRPC:
//
//   scaffold.Run(scaffold.Opts{
//       ServiceName: "payment-core",
//       Models: []any{...},
//       RegisterRoutes: func(app *App, cfg *Config) error {
//           // HTTP routes
//           handler.MountHTTP(app)
//           return nil
//       },
//       GRPC: &scaffold.GRPCOpts{
//           Addr: ":9090",
//           Register: func(srv *grpc.Server) {
//               pb.RegisterPaymentCoreServer(srv, paymentCoreImpl)
//           },
//       },
//   })
//
// 同一服务可以两者都有: HTTP 给 admin / merchant; gRPC 给内部服务间.

package scaffold

import (
	"context"
	"errors"
	"net"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// GRPCOpts gRPC server 配置.
type GRPCOpts struct {
	Addr     string                      // 默认 ":9090"
	Register func(srv *grpc.Server)      // 业务侧塞 service impl
	// 可选钩子
	UnaryInterceptors  []grpc.UnaryServerInterceptor
	StreamInterceptors []grpc.StreamServerInterceptor
}

// startGRPC 由 Run 内部调用. 返回 stop function.
func startGRPC(opts GRPCOpts, cfg *Config, log *zap.Logger) (stop func(), err error) {
	if opts.Addr == "" {
		opts.Addr = ":9090"
	}

	// 内置 interceptor: recover / logger / trace
	interceptors := append([]grpc.UnaryServerInterceptor{
		grpcRecoverUnary(log),
		grpcLoggerUnary(log),
		grpcTraceUnary(),
	}, opts.UnaryInterceptors...)

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(interceptors...),
		grpc.ChainStreamInterceptor(opts.StreamInterceptors...),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)

	// 业务 register
	if opts.Register != nil {
		opts.Register(srv)
	}

	// 标准 health check (gRPC health v1)
	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, healthSrv)

	lis, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, err
	}

	go func() {
		log.Info("gRPC listening", zap.String("addr", opts.Addr))
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Error("gRPC serve", zap.Error(err))
		}
	}()

	stop = func() {
		log.Info("gRPC graceful stop begin")
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(cfg.ShutdownTimeout):
			log.Warn("gRPC graceful stop timeout, forcing")
			srv.Stop()
		}
	}
	return stop, nil
}

// ── interceptors ──

func grpcRecoverUnary(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("grpc panic",
					zap.String("method", info.FullMethod),
					zap.Any("panic", r))
				err = status.Errorf(codes.Internal, "internal server error")
			}
		}()
		return handler(ctx, req)
	}
}

func grpcLoggerUnary(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		t0 := time.Now()
		resp, err := handler(ctx, req)
		code := codes.OK
		if err != nil {
			if s, ok := status.FromError(err); ok {
				code = s.Code()
			} else {
				code = codes.Unknown
			}
		}
		log.Info("grpc",
			zap.String("method", info.FullMethod),
			zap.String("code", code.String()),
			zap.Duration("dur", time.Since(t0)),
			zap.String("trace_id", TraceIDFromCtx(ctx)),
		)
		return resp, err
	}
}

// grpcTraceUnary — 把入站 trace_id 注入 ctx; 出站 client 同名 metadata 透传.
//
// 跟 HTTP 中间件的 X-Trace-Id 等价 (gRPC metadata "x-trace-id" — gRPC header 小写).
func grpcTraceUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		traceID := ""
		if v := md.Get("x-trace-id"); len(v) > 0 {
			traceID = v[0]
		}
		if traceID == "" {
			traceID = newTraceID()
		}
		ctx = WithTraceID(ctx, traceID)
		// 写到 outgoing metadata 透传到下游 grpc 调用
		ctx = metadata.AppendToOutgoingContext(ctx, "x-trace-id", traceID)
		return handler(ctx, req)
	}
}

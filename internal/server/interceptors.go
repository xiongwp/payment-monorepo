// Package server gRPC interceptors：recover / log / metrics / rate limit / auth
package server

import (
	"context"
	"runtime/debug"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/xiongwp/payment-core/internal/metrics"
)

// LoggingInterceptor 每次 RPC 进出都打日志：
//   IN  方法 + 完整 request payload（protojson）
//   OUT 方法 + 耗时 + status code + 完整 response payload 或 error
//
// 用 zap.Info 级别，prod / dev 两种 logger 都会输出（只是 prod 是 JSON 单行、
// dev 是人读多行）—— 排查问题时永远有迹可循。
func LoggingInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		start := time.Now()
		logger.Info("grpc IN",
			zap.String("method", info.FullMethod),
			zap.String("req", renderProto(req)),
		)
		resp, err := handler(ctx, req)
		dur := time.Since(start)
		if err != nil {
			st, _ := status.FromError(err)
			logger.Warn("grpc OUT",
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", st.Code().String()),
				zap.String("err", err.Error()))
		} else {
			logger.Info("grpc OUT",
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", "OK"),
				zap.String("resp", renderProto(resp)))
		}
		return resp, err
	}
}

func renderProto(v interface{}) string {
	if v == nil {
		return "null"
	}
	if m, ok := v.(proto.Message); ok {
		b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
		if err == nil {
			return string(b)
		}
	}
	return "<non-proto>"
}

func MetricsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := codes.OK.String()
		if err != nil {
			if st, ok := status.FromError(err); ok {
				code = st.Code().String()
			} else {
				code = codes.Unknown.String()
			}
		}
		metrics.GRPCRequestTotal.WithLabelValues(info.FullMethod, code).Inc()
		metrics.GRPCRequestDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

func AuthInterceptor(validTokens map[string]string, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if len(validTokens) == 0 || skip(info.FullMethod) {
			return handler(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		auth := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
		if !strings.HasPrefix(auth, "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		tok := strings.TrimPrefix(auth, "Bearer ")
		if _, ok := validTokens[tok]; !ok {
			logger.Debug("auth rejected", zap.String("method", info.FullMethod))
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		return handler(ctx, req)
	}
}

func RateLimitInterceptor(rps float64, burst int) grpc.UnaryServerInterceptor {
	if rps <= 0 {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	limiter := rate.NewLimiter(rate.Limit(rps), burst)
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !limiter.Allow() {
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(ctx, req)
	}
}

func RecoverInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in grpc handler",
					zap.String("method", info.FullMethod),
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())))
				err = status.Errorf(codes.Internal, "internal panic")
			}
		}()
		return handler(ctx, req)
	}
}

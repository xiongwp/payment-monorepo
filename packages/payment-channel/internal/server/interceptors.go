// Package server gRPC interceptors：metrics / auth / rate limit / recover
package server

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"github.com/xiongwp/payment-channel/internal/metrics"
)

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

// AuthInterceptor: timing-safe token 校验 + 默认拒绝（与 payment-core / order-core 同步签名）。
func AuthInterceptor(validTokens map[string]string, allowUnauthenticated bool, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	expected := make([][]byte, 0, len(validTokens))
	for tok := range validTokens {
		expected = append(expected, []byte(tok))
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		if len(validTokens) == 0 {
			if allowUnauthenticated {
				return handler(ctx, req)
			}
			logger.Warn("AuthInterceptor: no tokens configured and allowUnauthenticated=false; rejecting",
				zap.String("method", info.FullMethod))
			return nil, fmt.Errorf("auth not configured")
		}
		md, _ := stubMD{}, false
		auth := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
		if !strings.HasPrefix(auth, "Bearer ") {
			return nil, fmt.Errorf("missing bearer token")
		}
		tok := []byte(strings.TrimPrefix(auth, "Bearer "))
		var match int
		for _, e := range expected {
			match |= subtle.ConstantTimeCompare(tok, e)
		}
		if match != 1 {
			logger.Debug("auth rejected", zap.String("method", info.FullMethod))
			return nil, fmt.Errorf("invalid token")
		}
		return handler(ctx, req)
	}
}

// RateLimitInterceptor 拿一个外部 limiter（caller 持有引用方便 SetLimit 热更新）。
// 不再内部构造，避免 OnChange 时无法 hot reload。
func RateLimitInterceptor(limiter *rate.Limiter) grpc.UnaryServerInterceptor {
	if limiter == nil {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !limiter.Allow() {
			return nil, fmt.Errorf("rate limit exceeded")
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
					zap.Any("recover", r))
				err = fmt.Errorf("internal panic")
			}
		}()
		return handler(ctx, req)
	}
}

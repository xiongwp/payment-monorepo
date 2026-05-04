// Package server — gRPC interceptors 过渡层；基础实现在 pkg/grpcutil。
// 只保留 user-merchant-core 自己的 Prometheus 指标桥接。
package server

import (
	"google.golang.org/grpc"

	"github.com/xiongwp/user-merchant-core/internal/metrics"
	"github.com/xiongwp/user-merchant-core/pkg/grpcutil"
)

// LoggingOptions re-exported.
type LoggingOptions = grpcutil.LoggingOptions

// RecoverInterceptor / LoggingInterceptor / AuthInterceptor / RateLimitInterceptor
// 直接透传 pkg/grpcutil 的实现。
var (
	RecoverInterceptor   = grpcutil.RecoverInterceptor
	LoggingInterceptor   = grpcutil.LoggingInterceptor
	AuthInterceptor      = grpcutil.AuthInterceptor
	RateLimitInterceptor = grpcutil.RateLimitInterceptor
)

// MetricsInterceptor 把通用记录器桥到本服务的 Prometheus 计数器。
func MetricsInterceptor() grpc.UnaryServerInterceptor {
	return grpcutil.MetricsInterceptor(func(method, code string, durSec float64) {
		metrics.GRPCRequestTotal.WithLabelValues(method, code).Inc()
		metrics.GRPCRequestDuration.WithLabelValues(method).Observe(durSec)
	})
}

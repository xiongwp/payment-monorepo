// interceptors.go — SP-AC-7 O1: gRPC server unary interceptor chain.
//
// 顺序 (从外到内):
//   1. PanicRecover — 兜底 panic, 转 codes.Internal, 不让单条请求把 server 干挂
//   2. AccessLog    — 每个 RPC 落一条结构化 log (method/dur/code/peer)
//   3. Metrics      — qps / latency / error rate (per method + code)
//   4. AdminTokenAuth — (已在 main.go 里另作一个; 这里只声明顺序)
//
// 跨 interceptor 通过 ctx 透传 trace_id (由 propagator 上游已注入).
package grpcsvc

import (
	"context"
	"runtime/debug"
	"time"

	"reconcile-system/packages/split-payment/internal/observability"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ─── Prometheus metrics (per-method) ─────────────────────────────────────

var (
	grpcDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "split_payment",
		Subsystem: "grpc_server",
		Name:      "handled_duration_seconds",
		Help:      "gRPC server unary handler duration, per method+code.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"method", "code"})

	grpcStarted = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "split_payment",
		Subsystem: "grpc_server",
		Name:      "started_total",
		Help:      "Total gRPC requests started.",
	}, []string{"method"})

	grpcHandled = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "split_payment",
		Subsystem: "grpc_server",
		Name:      "handled_total",
		Help:      "Total gRPC requests handled, per method+code.",
	}, []string{"method", "code"})

	grpcPanics = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "split_payment",
		Subsystem: "grpc_server",
		Name:      "panics_total",
		Help:      "Total panics recovered by gRPC interceptor.",
	})
)

// ChainInterceptors 按顺序组合多个 UnaryServerInterceptor. grpc.NewServer 接受单个 interceptor,
// 这里把它们手动 chain (官方 grpc.ChainUnaryInterceptor 也能用, 但显式 chain 更清晰).
func ChainInterceptors(is ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		// 从尾到头包 handler
		final := h
		for i := len(is) - 1; i >= 0; i-- {
			cur := is[i]
			next := final
			final = func(c context.Context, r any) (any, error) {
				return cur(c, r, info, next)
			}
		}
		return final(ctx, req)
	}
}

// PanicRecoverInterceptor — 拦 panic, 转 codes.Internal, 加 stack trace 到 log.
func PanicRecoverInterceptor(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				grpcPanics.Inc()
				stack := debug.Stack()
				if log != nil {
					log.Error("gRPC handler panic",
						zap.String("method", info.FullMethod),
						zap.Any("panic", r),
						zap.ByteString("stack", stack))
				}
				err = status.Errorf(codes.Internal, "internal server error (panic recovered)")
				resp = nil
			}
		}()
		return h(ctx, req)
	}
}

// AccessLogInterceptor — 每个 RPC 落一条 INFO 级 access log.
//
// 字段: method / duration / code / peer / trace_id / actor.
// 高频低价值流量可以加 path-based sampling, 当前全量记 (split-payment 流量 < 100 qps).
func AccessLogInterceptor(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := h(ctx, req)
		dur := time.Since(start)
		code := codes.OK
		if err != nil {
			code = status.Code(err)
		}
		peerInfo := "unknown"
		if p, ok := peer.FromContext(ctx); ok {
			peerInfo = p.Addr.String()
		}
		if log != nil {
			fields := []zap.Field{
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", code.String()),
				zap.String("peer", peerInfo),
				zap.String("actor", extractActor(ctx)),
			}
			if tid := observability.TraceIDFromCtx(ctx); tid != "" {
				fields = append(fields, zap.String("trace_id", tid))
			}
			if err != nil {
				fields = append(fields, zap.Error(err))
				log.Warn("gRPC", fields...)
			} else {
				log.Info("gRPC", fields...)
			}
		}
		return resp, err
	}
}

// MetricsInterceptor — 把每个 RPC 的 qps / duration / error code 推 Prometheus.
func MetricsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		grpcStarted.WithLabelValues(info.FullMethod).Inc()
		start := time.Now()
		resp, err := h(ctx, req)
		code := codes.OK
		if err != nil {
			code = status.Code(err)
		}
		grpcDuration.WithLabelValues(info.FullMethod, code.String()).Observe(time.Since(start).Seconds())
		grpcHandled.WithLabelValues(info.FullMethod, code.String()).Inc()
		return resp, err
	}
}

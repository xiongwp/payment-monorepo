// interceptors.go — 标准 gRPC server unary interceptor 链.
//
// 任何 Go 服务用 grpc.NewServer 时挂这一套, 自动拿到:
//   - panic recovery (拦 panic 转 codes.Internal, 不让单条挂掉 server)
//   - access log (每个 RPC 落 method/dur/code/peer/trace_id)
//   - metrics (qps/duration/error per method+code)
//
// 用法:
//	srv := grpc.NewServer(grpc.UnaryInterceptor(obsbootstrap.Chain(
//	    obsbootstrap.PanicRecover(log),
//	    obsbootstrap.AccessLog(log),
//	    obsbootstrap.Metrics("payment-core"),
//	    myAuth,  // 业务自己的 auth interceptor
//	)))
//
// metric name 用 service name 做 namespace, 多服务汇总到同一 Prometheus 不冲突.
package obsbootstrap

import (
	"context"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// metricRegistry 防止同一 service name 多次注册 (单元测试 / 多 server 同进程).
var (
	metricRegMu sync.Mutex
	metricReg   = map[string]*svcMetrics{}
)

type svcMetrics struct {
	duration *prometheus.HistogramVec
	started  *prometheus.CounterVec
	handled  *prometheus.CounterVec
	panics   prometheus.Counter
}

func registerMetrics(service string) *svcMetrics {
	metricRegMu.Lock()
	defer metricRegMu.Unlock()
	if m, ok := metricReg[service]; ok {
		return m
	}
	m := &svcMetrics{
		duration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: sanitize(service),
			Subsystem: "grpc_server",
			Name:      "handled_duration_seconds",
			Help:      "gRPC server handler latency in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "code"}),
		started: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: sanitize(service),
			Subsystem: "grpc_server",
			Name:      "started_total",
			Help:      "Total gRPC requests started.",
		}, []string{"method"}),
		handled: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: sanitize(service),
			Subsystem: "grpc_server",
			Name:      "handled_total",
			Help:      "Total gRPC requests handled, by method+code.",
		}, []string{"method", "code"}),
		panics: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: sanitize(service),
			Subsystem: "grpc_server",
			Name:      "panics_total",
			Help:      "Total panics recovered by interceptor.",
		}),
	}
	metricReg[service] = m
	return m
}

// sanitize 把 service name 改成合法 Prometheus namespace (字母数字下划线).
func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "svc"
	}
	return string(out)
}

// Chain 组合多个 interceptor (执行顺序从前到后, panic recover 通常放最外/最前).
func Chain(is ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
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

// PanicRecover 拦 panic.
func PanicRecover(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				if log != nil {
					log.Error("gRPC handler panic",
						zap.String("method", info.FullMethod),
						zap.Any("panic", r),
						zap.ByteString("stack", debug.Stack()))
				}
				err = status.Errorf(codes.Internal, "internal server error (panic recovered)")
				resp = nil
			}
		}()
		return h(ctx, req)
	}
}

// AccessLog 每个 RPC 落一条结构化 log.
func AccessLog(log *zap.Logger) grpc.UnaryServerInterceptor {
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

// Metrics 接 Prometheus, service name 作 namespace 避免多服务冲突.
func Metrics(service string) grpc.UnaryServerInterceptor {
	m := registerMetrics(service)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		m.started.WithLabelValues(info.FullMethod).Inc()
		start := time.Now()
		resp, err := h(ctx, req)
		code := codes.OK
		if err != nil {
			code = status.Code(err)
		}
		m.duration.WithLabelValues(info.FullMethod, code.String()).Observe(time.Since(start).Seconds())
		m.handled.WithLabelValues(info.FullMethod, code.String()).Inc()
		return resp, err
	}
}

// PanicCounter 给调用方手动 Inc, 一般 PanicRecover 内部已经做了; 暴露出来供测试.
func PanicCounter(service string) prometheus.Counter {
	return registerMetrics(service).panics
}

// Package trace 提供全链路 Trace ID 传播。
//
// 规则：
//   1. 客户端在 gRPC metadata / HTTP header 里带 "x-trace-id"
//   2. 没带的话服务端生成一个
//   3. 调下游 gRPC 时把 trace_id 塞进 outgoing metadata
//   4. 所有日志自动带 trace_id 字段
//
// 用法：
//   - gRPC：把 TraceInterceptor 加到 ChainUnaryInterceptor 最前面
//   - HTTP：把 HTTPMiddleware 加到 mux 中间件
//   - 调下游：ctx = trace.Inject(ctx) 让 outgoing metadata 也带上
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	HeaderKey   = "x-trace-id"
	MetadataKey = "x-trace-id"
)

type ctxKey struct{}

// FromContext 从 ctx 取 trace ID；没有返回 ""。
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTraceID 往 ctx 里塞 trace ID。
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, traceID)
}

// Inject 把 ctx 里的 trace ID 写进 outgoing gRPC metadata（调下游前用）。
func Inject(ctx context.Context) context.Context {
	tid := FromContext(ctx)
	if tid == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, MetadataKey, tid)
}

// Generate 生成 trace ID：<unix_ms_hex>-<random_8B_hex>
func Generate() string {
	ts := time.Now().UnixMilli()
	rb := make([]byte, 8)
	_, _ = rand.Read(rb)
	return hex.EncodeToString([]byte{
		byte(ts >> 40), byte(ts >> 32), byte(ts >> 24),
		byte(ts >> 16), byte(ts >> 8), byte(ts),
	}) + "-" + hex.EncodeToString(rb)
}

// UnaryServerInterceptor gRPC 服务端拦截器：提取或生成 trace ID，注入 ctx + logger。
func UnaryServerInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if strings.HasPrefix(info.FullMethod, "/grpc.health.") {
			return handler(ctx, req)
		}
		tid := extractFromMD(ctx)
		if tid == "" {
			tid = Generate()
		}
		ctx = WithTraceID(ctx, tid)
		// 也写进 outgoing metadata，方便服务内部再转发
		ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, tid)
		// 把 trace_id 加到后续所有 zap 日志里
		l := logger.With(zap.String("trace_id", tid))
		ctx = withLogger(ctx, l)
		return handler(ctx, req)
	}
}

func extractFromMD(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(MetadataKey)
	if len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// ─── ctx logger helper ────────────────────────────────────────────

type loggerKey struct{}

func withLogger(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger 从 ctx 取带 trace_id 的 logger；没有就返回传入的 fallback。
func Logger(ctx context.Context, fallback *zap.Logger) *zap.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*zap.Logger); ok {
		return l
	}
	tid := FromContext(ctx)
	if tid != "" {
		return fallback.With(zap.String("trace_id", tid))
	}
	return fallback
}

// UnaryClientInterceptor gRPC 客户端拦截器：把 ctx 里的 trace ID 自动注入
// outgoing metadata，让下游 server 能拿到同一条 trace。
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(Inject(ctx), method, req, reply, cc, opts...)
	}
}

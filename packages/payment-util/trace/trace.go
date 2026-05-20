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
	"time"

	"go.uber.org/zap"
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

// Inject 已弃用 — 老 gRPC 时代把 trace ID 写进 outgoing metadata 的辅助.
// Kitex 切换后由 kitexutil.TraceClientMW 自动透传 (TTHeader / metainfo).
// 保留 0-cost 空实现避免 caller import cycle.
func Inject(ctx context.Context) context.Context { return ctx }

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

// UnaryServerInterceptor / extractFromMD 已删 — gRPC interceptor 不再适用 Kitex.
// 等价物在 kitexutil.TraceMW (Kitex middleware), 由 cmd/server/main.go 通过
// server.WithMiddleware(...) 接入. Trace ID 从 TTHeader/metainfo 透传.

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

// UnaryClientInterceptor 已删 — gRPC interceptor 不适用 Kitex.
// 等价物在 kitexutil.TraceClientMW, 通过 client.WithMiddleware(...) 接入.

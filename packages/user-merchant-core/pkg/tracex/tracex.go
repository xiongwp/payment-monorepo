// Package tracex 提供全链路 Trace ID 传播 — 跟 payment-util/trace 一致.
//
// 老 gRPC interceptor (UnaryServerInterceptor / Inject / extractFromMD) 已删,
// Kitex 切换后由 TTHeader / metainfo 自动透传 trace_id, 业务路径只用:
//   - FromContext / WithTraceID — ctx 读写
//   - Generate                  — 服务端没收到 trace_id 时生成
//   - Logger                    — 带 trace_id 的 zap.Logger 包装
package tracex

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

// FromContext 从 ctx 取 trace ID; 没有返回 "".
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKey{}).(string); ok {
		return v
	}
	return ""
}

// WithTraceID 往 ctx 里塞 trace ID.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, traceID)
}

// Inject 已弃用 — Kitex 切换后 TTHeader / metainfo 自动透传; 保留 no-op 兼容旧 caller.
func Inject(ctx context.Context) context.Context { return ctx }

// Generate 生成 trace ID: <unix_ms_hex>-<random_8B_hex>.
func Generate() string {
	ts := time.Now().UnixMilli()
	rb := make([]byte, 8)
	_, _ = rand.Read(rb)
	return hex.EncodeToString([]byte{
		byte(ts >> 40), byte(ts >> 32), byte(ts >> 24),
		byte(ts >> 16), byte(ts >> 8), byte(ts),
	}) + "-" + hex.EncodeToString(rb)
}

type loggerKey struct{}

// WithLogger 给业务路径手动注入带 trace_id 的 logger (Kitex MW 接通后由 MW 调).
func WithLogger(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger 从 ctx 取带 trace_id 的 logger; 没有就返回 fallback.
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

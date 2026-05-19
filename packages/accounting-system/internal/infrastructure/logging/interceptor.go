package logging

// 老 gRPC UnaryServerInterceptor 全删 — Kitex 切换后用 server.WithMiddleware
// (kitexutil.LogMW). 这里保留:
//   - WithTraceID / TraceIDFromCtx — ctx 读写 trace_id
//   - LoggerFromCtx                — 带 trace_id 的 zap.Logger
//   - newTraceID                   — 生成 8-byte hex trace id

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// traceIDKey 是 ctx 中存放 trace_id 的私有 key 类型，避免与其他包的 ctx key 冲突。
type traceIDKey struct{}

// TraceIDHeader 是客户端可以传入的现成 trace_id（典型场景：网关在入口生成，
// 沿调用链向下传递）。也作为响应头返回给客户端便于客户端日志关联。
const TraceIDHeader = "x-trace-id"

// WithTraceID 把 trace_id 注入 ctx。下游通过 TraceIDFromCtx(ctx) 取出。
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey{}, id)
}

// TraceIDFromCtx 从 ctx 提取 trace_id。未设置时返回空串。
func TraceIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey{}).(string); ok {
		return v
	}
	return ""
}

// LoggerFromCtx 返回带 trace_id 字段的子 logger。base 为 nil 时返回 nil（避免 panic）。
func LoggerFromCtx(ctx context.Context, base *zap.Logger) *zap.Logger {
	if base == nil {
		return nil
	}
	id := TraceIDFromCtx(ctx)
	if id == "" {
		return base
	}
	return base.With(zap.String("trace_id", id))
}

// NewTraceID 生成 16-hex-char (8-byte) trace id (导出, 让 Kitex MW 在新位置调).
func NewTraceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

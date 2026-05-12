// trace.go — 全局 trace_id propagation.
//
// 模式:
//   - HTTP 入站: X-Trace-Id header → ctx → 出站 HTTP client / 出站 gRPC metadata
//   - gRPC 入站: x-trace-id metadata → ctx → 出站 HTTP / gRPC
//
// 跟 OpenTelemetry traceparent (W3C) 不同 — 这是简化 trace_id, 给 reconplatform
// 跨服务关联用. 真生产建议两套都跑 (traceparent OTel + 兼容 x-trace-id).

package scaffold

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/gofiber/fiber/v2"
)

type traceCtxKey struct{}

// WithTraceID 把 trace_id 写入 ctx.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, traceID)
}

// TraceIDFromCtx 取 trace_id; 没有返空.
func TraceIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(traceCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// newTraceID 生成 16-byte hex (跟 W3C traceparent trace-id 同长度).
func newTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// fiberTraceMW — 自动: 入站读 X-Trace-Id, 没有生成新的, 写 ctx + 响应头.
//
// 跟 grpcTraceUnary 配对. server.go 在 NewApp 自动装这个中间件.
func fiberTraceMW() fiber.Handler {
	return func(c *fiber.Ctx) error {
		traceID := c.Get("X-Trace-Id")
		if traceID == "" {
			// 也兼容 W3C traceparent (00-<trace_id>-<span_id>-<flags>)
			if tp := c.Get("Traceparent"); len(tp) >= 35 {
				traceID = tp[3:35]
			}
		}
		if traceID == "" {
			traceID = newTraceID()
		}
		c.Set("X-Trace-Id", traceID)
		// 写 fiber locals 和 std ctx
		c.Locals("trace_id", traceID)
		c.SetUserContext(WithTraceID(c.UserContext(), traceID))
		return c.Next()
	}
}

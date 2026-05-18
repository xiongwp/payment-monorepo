// trace.go — SP-AC-7 P10: 最小可用 OpenTelemetry trace 接入.
//
// 目标:
//   - gRPC server/client 注入 stats handler, 自动透传 trace context (W3C traceparent)
//   - zap 日志自动带 trace_id / span_id (从 ctx 抓)
//
// **非目标** (留 Phase 3):
//   - OTLP exporter (推 Jaeger/Tempo); 本最小实现只有 in-process propagation
//   - 业务级 span 注解 (e.g. translator.Translate 切个 span)
//   - sampling 配置
//
// 启用方式: main.go 调 ShutdownFunc := observability.InitTracer(serviceName)
// 关闭时 defer ShutdownFunc(ctx).
package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// InitTracer — 安装 TracerProvider + Propagator.
//
// 当前用默认 noop TracerProvider (不实际 export), 只确保 ctx 传递正常. 后续接 OTLP
// 时换成 otlptracegrpc.New(...) → tracesdk.NewBatchSpanProcessor.
//
// 返 cleanup func; 进程退出前调用.
func InitTracer(_ string) func(context.Context) error {
	// W3C TraceContext + Baggage 这两个 propagator 几乎所有现代 OTel SDK 都支持.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// 这里不显式 SetTracerProvider → 用默认 noop. 当真正接 exporter 时:
	//   tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	//   otel.SetTracerProvider(tp)
	return func(_ context.Context) error { return nil }
}

// LogFromCtx 给 zap.Logger 附上当前 ctx 的 trace_id / span_id (若有).
//
// 用法:
//   log := observability.LogFromCtx(ctx, baseLog)
//   log.Info("...")  // 自动带 trace_id field
//
// span 不存在时返原 logger.
func LogFromCtx(ctx context.Context, base *zap.Logger) *zap.Logger {
	if base == nil {
		return nil
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return base
	}
	return base.With(
		zap.String("trace_id", sc.TraceID().String()),
		zap.String("span_id", sc.SpanID().String()),
	)
}

// TraceIDFromCtx 拿 trace_id 字符串; 没有 → "".
func TraceIDFromCtx(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

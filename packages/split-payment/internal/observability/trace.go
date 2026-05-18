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
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// InitTracer — SP-AC-7 PH3-1: 安装 TracerProvider + Propagator.
//
// 行为:
//   - env OTEL_EXPORTER_OTLP_ENDPOINT 配了 → 接 OTLP gRPC exporter (推 collector → Tempo/Jaeger)
//   - 否则 → 退化为 noop tracer (仅 in-process propagation, 无外送)
//   - W3C TraceContext + Baggage propagator 永远装上 (gRPC 跨服务链路 + zap 注入用)
//
// 真实生产 OTEL_EXPORTER_OTLP_ENDPOINT 一般是 otel-collector sidecar: `otel-collector.observability:4317`
// 也支持 OTEL_SERVICE_NAME, OTEL_EXPORTER_OTLP_HEADERS 等标准 env.
//
// 返 cleanup func; 进程 SIGINT/SIGTERM 时 defer 调.
func InitTracer(serviceName string) func(context.Context) error {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		// 没配 collector → noop tracer, 但 propagator 还在, ctx 仍能传 trace_id (上游有就用上游的).
		return func(_ context.Context) error { return nil }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exp, err := otlptrace.New(ctx,
		otlptracegrpc.NewClient(
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(), // 生产应换 WithTLSCredentials
			otlptracegrpc.WithTimeout(5*time.Second),
		),
	)
	if err != nil {
		// exporter 起不来仍能用 noop tracer, 不阻塞进程启动.
		return func(_ context.Context) error { return nil }
	}
	res, _ := resource.New(context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(os.Getenv("BUILD_VERSION")),
			semconv.DeploymentEnvironment(os.Getenv("DEPLOY_ENV")),
		),
	)
	tp := sdktrace.NewTracerProvider(
		// BatchSpanProcessor: 批量推, 减网络开销. 默认 30s flush.
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		// 简单按 % 采样; 生产可换 tail-based.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown
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

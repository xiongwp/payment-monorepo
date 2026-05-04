// Package trace — otel.go：OpenTelemetry 初始化。
//
// 和 trace.go（自定义 x-trace-id 传播）共存。用法：
//   - trace.go 仍然负责注入 logger 的 trace_id 字段（grep 友好）
//   - otel 负责往 Jaeger/Tempo 推 span 树（可视化链路）
//   - gRPC server/client 拦截器同时挂自定义 trace + otelgrpc StatsHandler
//
// 通过 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量启用：
//   OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector:4317 ./server
//   → OTLP gRPC exporter，batch 发 span 到 collector
//
// endpoint 为空时 Init 返回 no-op shutdown，零副作用（生产默认不启用）。
package trace

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// InitOTel 按 endpoint 初始化 global tracer provider。
// 返回 shutdown 函数，进程退出前调用以 flush pending span。
// endpoint 空 → no-op 模式（返回空 shutdown，不建立 grpc 连接）。
func InitOTel(ctx context.Context, serviceName, endpoint string) (func(context.Context) error, error) {
	if endpoint == "" {
		if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
			endpoint = v
		}
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	res, err := resource.Merge(resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, // W3C traceparent
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

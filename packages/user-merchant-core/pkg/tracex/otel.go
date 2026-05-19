package tracex

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// OTelConfig OTel 出口参数。Endpoint 为空 → 不启用（tracex 继续只做 trace_id 串联）。
// 典型生产配置：Endpoint=collector:4317 + SampleRatio=0.1。
type OTelConfig struct {
	Endpoint     string
	ServiceName  string
	Env          string
	SampleRatio  float64 // 0 = never, 1 = always；[0, 1]
	ExportTimeout time.Duration
	Insecure     bool
}

// InitOTel 初始化全局 tracer provider。返回的 shutdown 应在 main 的 OnStop 调。
// Endpoint 为空时返回 noop shutdown，业务调用 otel.Tracer(...) 拿到 noop tracer
// 不会报错也不会导出。
func InitOTel(ctx context.Context, cfg OTelConfig) (shutdown func(context.Context) error, err error) {
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 10 * time.Second
	}
	if cfg.SampleRatio <= 0 {
		cfg.SampleRatio = 0.1
	} else if cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithTimeout(cfg.ExportTimeout),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otel exporter: %w", err)
	}

	res, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			attribute.String("deployment.environment", cfg.Env),
		),
	)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRatio),
		)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// OTelServerInterceptor / OTelClientInterceptor 已删 — gRPC interceptor 不适用 Kitex.
// 等价 Kitex MW: 用 cloudwego/kitex obs 模块 (kitexutil.OTelMW) — 通过
// server.WithMiddleware(...) / client.WithMiddleware(...) 接入.

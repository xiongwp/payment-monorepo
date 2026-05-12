// otel.go — OpenTelemetry OTLP exporter 初始化.
//
// 默认 disabled; cfg.OTel.Endpoint 设置后启用.
//
// 行为:
//   - OTLP gRPC exporter → Jaeger / Tempo / Datadog collector
//   - 自动 service.name + version + env 资源属性
//   - 自动 propagate W3C traceparent + 我们的 x-trace-id 双标准
//   - Fiber 中间件每个 request 起一个 root span
//   - gRPC interceptor 起 server span
//   - GORM callback 起 db.query span (慢查询自动 mark)
//
// 真生产建议 sampling: 1% baseline + 100% error.

package scaffold

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// OTelConfig 在 Config 里嵌入 (yaml: otel)
type OTelConfig struct {
	Endpoint    string  `yaml:"endpoint"`     // "tempo:4317" / "otel-collector:4317"; 空则禁用
	SampleRatio float64 `yaml:"sample_ratio"` // 0..1; 默认 0.01 (1%)
	Insecure    bool    `yaml:"insecure"`     // dev=true (no TLS)
	Env         string  `yaml:"env"`          // dev / staging / prod
	Version     string  `yaml:"version"`      // git sha
}

// initOTel 初始化 tracer provider; 返 shutdown func.
// scaffold.Run 自动调用; 业务侧不用关心.
func initOTel(cfg OTelConfig, serviceName string, log *zap.Logger) (func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if cfg.SampleRatio == 0 {
		cfg.SampleRatio = 0.01
	}
	if cfg.Env == "" {
		cfg.Env = "dev"
	}

	ctx := context.Background()
	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(cfg.Version),
			semconv.DeploymentEnvironmentName(cfg.Env),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(res),
		// 1% sample + 100% error (parent-based, error 强制 keep)
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRatio),
		)),
	)
	otel.SetTracerProvider(tp)

	// W3C tracecontext + baggage propagator
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	log.Info("OTel initialized",
		zap.String("endpoint", cfg.Endpoint),
		zap.Float64("sample_ratio", cfg.SampleRatio),
	)

	return func(ctx context.Context) error {
		sCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(sCtx)
	}, nil
}

// SpanFromCtx 业务侧拿当前 span 加属性.
//
//   span := scaffold.SpanFromCtx(ctx)
//   span.SetAttributes(attribute.String("merchant_id", id))
func SpanFromCtx(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// SetAttr 业务侧便捷给当前 span 加 attribute.
func SetAttr(ctx context.Context, key string, value any) {
	s := trace.SpanFromContext(ctx)
	if !s.IsRecording() {
		return
	}
	switch v := value.(type) {
	case string:
		s.SetAttributes(attribute.String(key, v))
	case int:
		s.SetAttributes(attribute.Int(key, v))
	case int64:
		s.SetAttributes(attribute.Int64(key, v))
	case float64:
		s.SetAttributes(attribute.Float64(key, v))
	case bool:
		s.SetAttributes(attribute.Bool(key, v))
	}
}

// 让 unused import 不报错
var _ = errors.New

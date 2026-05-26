// tracing_test.go — 验证 Kitex 入站 trace MW.
//
// 三个核心断言:
//  1. 入站请求带 traceparent → 创建的 server span 是这个 traceparent 的 child
//     (TraceID 相同, parent SpanID = 入站 traceparent 的 SpanID)
//  2. handler 内 ctx 上能拿到当前 span (otel.SpanFromContext 不返 noop)
//  3. handler 返 error → server span.Status = Error
//
// 用 OTel SDK 自带的 tracetest.NewSpanRecorder() (InMemoryExporter 等价物) — 不
// 走网络, 同步抓 span.
package server

import (
	"context"
	"errors"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// setupTestTracing 注册一个 in-memory recorder 当 global tracer provider,
// 返 SpanRecorder + cleanup. 测试间互不污染.
func setupTestTracing(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	// AlwaysSample: 测试里不能丢任何 span.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	cleanup := func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	}
	return sr, cleanup
}

// TestTracingServerMW_ExtractsParentTraceparent 入站 traceparent → server span
// 必须是 caller 的 child (同 TraceID, parent SpanID 匹配).
func TestTracingServerMW_ExtractsParentTraceparent(t *testing.T) {
	sr, cleanup := setupTestTracing(t)
	defer cleanup()

	// 构造一个合法的 W3C traceparent: version-traceid-spanid-flags.
	// 16 byte traceID + 8 byte spanID. flags=01 表示已采样.
	const traceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	ctx := context.Background()
	ctx = metainfo.WithValue(ctx, "traceparent", traceparent)

	called := false
	mw := TracingServerMW()
	ep := mw(func(ctx context.Context, req, resp interface{}) error {
		called = true
		// 断言 2: handler 里能从 ctx 拿到当前 span
		span := oteltrace.SpanFromContext(ctx)
		if !span.SpanContext().IsValid() {
			t.Error("handler ctx 上没有 valid span — extract / Start 链路断了")
		}
		return nil
	})

	if err := ep(ctx, nil, nil); err != nil {
		t.Fatalf("MW returned err: %v", err)
	}
	if !called {
		t.Fatal("next handler 没被调用")
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 server span, got %d", len(spans))
	}
	srv := spans[0]

	// 断言 1: TraceID 跟 caller 一致
	wantTraceID, _ := oteltrace.TraceIDFromHex("0af7651916cd43dd8448eb211c80319c")
	if srv.SpanContext().TraceID() != wantTraceID {
		t.Errorf("trace id 没继承: got %s want %s",
			srv.SpanContext().TraceID(), wantTraceID)
	}
	// 断言 1b: parent SpanID 是 caller 的 spanID
	wantParentSpanID, _ := oteltrace.SpanIDFromHex("b7ad6b7169203331")
	if srv.Parent().SpanID() != wantParentSpanID {
		t.Errorf("parent span id 不对: got %s want %s",
			srv.Parent().SpanID(), wantParentSpanID)
	}
	// 断言: SpanKind = Server
	if srv.SpanKind() != oteltrace.SpanKindServer {
		t.Errorf("span kind want Server, got %v", srv.SpanKind())
	}
}

// TestTracingServerMW_NoTraceparent_CreatesRootSpan 入站没 traceparent →
// MW 仍然创建一个 root span (parent 不 valid 但 TraceID 自己生成).
func TestTracingServerMW_NoTraceparent_CreatesRootSpan(t *testing.T) {
	sr, cleanup := setupTestTracing(t)
	defer cleanup()

	mw := TracingServerMW()
	ep := mw(func(ctx context.Context, req, resp interface{}) error {
		// 这里也确认 ctx 上有 span, 后续业务 (engine.Evaluate) 的子 span 才能挂上
		if !oteltrace.SpanFromContext(ctx).SpanContext().IsValid() {
			t.Error("root span 没塞进 ctx")
		}
		return nil
	})

	if err := ep(context.Background(), nil, nil); err != nil {
		t.Fatalf("MW err: %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	// root span: parent 应该 invalid
	if spans[0].Parent().IsValid() {
		t.Errorf("没 traceparent 时不应有 valid parent, got %v", spans[0].Parent())
	}
	if !spans[0].SpanContext().TraceID().IsValid() {
		t.Errorf("root span 自己的 traceID 应该有效")
	}
}

// TestTracingServerMW_HandlerError_SetsStatus handler 返 error → span 状态 Error.
func TestTracingServerMW_HandlerError_SetsStatus(t *testing.T) {
	sr, cleanup := setupTestTracing(t)
	defer cleanup()

	wantErr := errors.New("simulated handler failure")
	mw := TracingServerMW()
	ep := mw(func(ctx context.Context, req, resp interface{}) error {
		return wantErr
	})

	if err := ep(context.Background(), nil, nil); err != wantErr {
		t.Fatalf("MW 没透传 handler error: got %v", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("span status code want Error, got %v", spans[0].Status().Code)
	}
	if len(spans[0].Events()) == 0 {
		t.Errorf("RecordError 应该产生一个 exception event")
	}
}

// TestTracingServerMW_NilSafe propagator nil / tracer nil 时 MW 也不能 panic.
// (实际 otel 包对 nil 有 noop 兜底, 这里只是双保险.)
func TestTracingServerMW_NilSafe(t *testing.T) {
	// 不调 setupTestTracing — 走 otel 默认 noop provider.
	mw := TracingServerMW()
	ep := mw(func(ctx context.Context, req, resp interface{}) error {
		return nil
	})
	if err := ep(context.Background(), nil, nil); err != nil {
		t.Fatalf("noop tracer 下也不能返 err, got %v", err)
	}
}

// TestMetainfoCarrier_GetSetKeys carrier 三个方法基本契约.
func TestMetainfoCarrier_GetSetKeys(t *testing.T) {
	ctx := context.Background()
	// Set 用 persistent — 后续 GetPersistentValue 拿得到
	c := metainfoCarrier{ctx: &ctx}
	c.Set("traceparent", "00-aa-bb-01")
	if got := c.Get("traceparent"); got != "00-aa-bb-01" {
		t.Errorf("Get round-trip 失败: got %q", got)
	}

	// transient 路径也能读
	ctx2 := metainfo.WithValue(context.Background(), "x-custom", "v1")
	c2 := metainfoCarrier{ctx: &ctx2}
	if got := c2.Get("x-custom"); got != "v1" {
		t.Errorf("transient Get 失败: got %q", got)
	}

	// 不存在的 key 返空, 不 panic
	if got := c2.Get("not-set"); got != "" {
		t.Errorf("missing key want empty, got %q", got)
	}

	// Keys 至少包含写过的 key
	c2.Set("traceparent", "x")
	keys := c2.Keys()
	found := false
	for _, k := range keys {
		if k == "traceparent" {
			found = true
		}
	}
	if !found {
		t.Errorf("Keys 没列出已 set 的 traceparent; got %v", keys)
	}

	// nil ctx 不 panic
	cNil := metainfoCarrier{ctx: nil}
	if got := cNil.Get("any"); got != "" {
		t.Errorf("nil ctx 应返空; got %q", got)
	}
	cNil.Set("any", "v") // 不能 panic
	if cNil.Keys() != nil {
		t.Errorf("nil ctx Keys 应返 nil")
	}
}

// TestInjectTraceContext 出站辅助: ctx 有 span → Inject 写 traceparent 到 metainfo.
func TestInjectTraceContext(t *testing.T) {
	_, cleanup := setupTestTracing(t)
	defer cleanup()

	tracer := otel.Tracer("test")
	ctx, span := tracer.Start(context.Background(), "outbound")
	defer span.End()

	ctx = injectTraceContext(ctx)
	// metainfo persistent 应有 traceparent
	if v, ok := metainfo.GetPersistentValue(ctx, "traceparent"); !ok || v == "" {
		t.Errorf("injectTraceContext 没写 traceparent 到 metainfo; got ok=%v v=%q", ok, v)
	}
}

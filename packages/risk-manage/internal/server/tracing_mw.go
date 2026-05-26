// tracing_mw.go — Kitex 入站 OpenTelemetry trace middleware.
//
// 干两件事:
//  1. 从 Kitex metainfo (TTHeader 透传过来) 提取 W3C TraceContext (traceparent /
//     tracestate / baggage), Extract 到 ctx —— 这样如果 caller 自己也跑了 OTel
//     instrumentation, 它的 span 会成为 server span 的 parent.
//  2. otel.Tracer.Start 一个 "kitex.server.<method>" span, 把 span ctx 往下传给
//     业务 handler. engine.Evaluate / mlscore / ipintel 那些 otel.Tracer(...).Start
//     的子 span 自动挂到这条 server span 下面 → 完整链路图.
//
// 不要装到 client 侧 — risk-manage 没有出站 Kitex 调用 (mlscore/ipintel 走 HTTP,
// engine 内部全 in-process). 后续若引入下游 Kitex client 再加 client MW.
//
// 这个 MW 本质上是 payment-util/kitexutil 该提的 TraceMW, 但 payment-util 还没
// port 完 (见 packages/payment-util/trace/trace.go:60); 先在 risk-manage 本地写,
// 等 payment-util.kitexutil.TraceMW 上线再切过去.
package server

import (
	"context"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// tracerName 给 otel.Tracer 的 instrumentation library 名. 跟 engine/engine.go
// 的 "risk-manage/engine" 同前缀, 在 backend (Jaeger/Tempo) 容易识别这是 server
// 入口 span.
const tracerName = "risk-manage/server"

// metainfoCarrier 让 Kitex metainfo (TTHeader 携带的 key/value) 适配 OTel
// TextMapCarrier 接口 — server 侧 Extract / client 侧 Inject 都用它.
//
// Read 路径 (server Extract): 同时尝试 GetValue (transient) 和 GetPersistentValue
// (persistent). caller 用哪种都能读到; 多数 OTel SDK 写 traceparent 用 transient.
//
// Write 路径 (client Inject): 写 persistent — 保证 traceparent 在 RPC 链 (含级联
// 调用) 始终透传, 不会跳过中间一跳 (gateway → A → B 时 A 必须把 traceparent
// 传给 B).
type metainfoCarrier struct {
	ctx *context.Context
}

// Get 实现 propagation.TextMapCarrier.
//
// 先看 transient (大部分 OTel propagator 默认写这里), 再 fallback persistent —
// 跟 Kitex contrib obs-opentelemetry 的实现一致. 没有时返 "" 让上游 propagator
// 知道该 key 缺失.
func (c metainfoCarrier) Get(key string) string {
	if c.ctx == nil || *c.ctx == nil {
		return ""
	}
	if v, ok := metainfo.GetValue(*c.ctx, key); ok && v != "" {
		return v
	}
	if v, ok := metainfo.GetPersistentValue(*c.ctx, key); ok && v != "" {
		return v
	}
	return ""
}

// Set 实现 propagation.TextMapCarrier. 写 persistent 保证级联透传.
//
// 注意 Set 修改的是 *c.ctx, 调用方拿到新 ctx 必须用 metainfoCarrier{&ctx} 的形式
// 传进来; 否则 ctx 在 Inject 之后是旧的.
func (c metainfoCarrier) Set(key, value string) {
	if c.ctx == nil || *c.ctx == nil {
		return
	}
	*c.ctx = metainfo.WithPersistentValue(*c.ctx, key, value)
}

// Keys 实现 propagation.TextMapCarrier. 合并 transient + persistent 全集.
//
// 给 propagator.Fields() 调用; 用来判断"这个 carrier 上有哪些 key 待 inject/extract".
// 顺序无关紧要 — propagator 自己只关心特定字段名 (traceparent / tracestate / baggage).
func (c metainfoCarrier) Keys() []string {
	if c.ctx == nil || *c.ctx == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for k := range metainfo.GetAllValues(*c.ctx) {
		seen[k] = struct{}{}
	}
	for k := range metainfo.GetAllPersistentValues(*c.ctx) {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	return keys
}

// TracingServerMW 入站 trace middleware.
//
// 执行流程:
//
//  1. propagator.Extract: 从 metainfo 读 traceparent → ctx 携带 caller span context
//  2. tracer.Start: 用 caller span 当 parent 起一个 SERVER kind span
//     (span name = "kitex.server.<method>", e.g. "kitex.server.Screen")
//  3. next(ctx, req, resp): 业务 handler 跑, 子 span 自动继承
//  4. defer span.End + 错误时 RecordError + SetStatus(Error)
//
// 防御:
//   - otel.GetTextMapPropagator() 内部就有 noop fallback, 不会返 nil; 我们这里
//     再做一层 ok-to-be-noop 兜底.
//   - 没装 TracerProvider 时 otel.Tracer 返 noop tracer, span.End 是 no-op,
//     不影响业务路径.
//   - rpcinfo.GetRPCInfo 可能为空 (Kitex 内部异常状态), 用 "unknown" 占位避免
//     SetAttributes panic.
func TracingServerMW() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp interface{}) error {
			propagator := otel.GetTextMapPropagator()
			// Extract: caller 的 traceparent (如果有) → ctx.
			// Carrier 读 *ctx 的指针, 但 Extract 只读不写, 所以这里直接传 ctx 就行;
			// 用指针形式是为了 Set 路径 (client inject 时) 一致.
			ctxPtr := ctx
			ctx = propagator.Extract(ctx, metainfoCarrier{ctx: &ctxPtr})

			// 取 RPC method 名做 span name. rpcinfo.To().Method() = "Screen" / "BulkScreen" 等.
			method := "unknown"
			caller := ""
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if m := ri.To(); m != nil && m.Method() != "" {
					method = m.Method()
				}
				if f := ri.From(); f != nil {
					caller = f.ServiceName()
				}
			}

			tracer := otel.Tracer(tracerName)
			ctx, span := tracer.Start(ctx, "kitex.server."+method,
				oteltrace.WithSpanKind(oteltrace.SpanKindServer),
				oteltrace.WithAttributes(
					semconv.RPCSystemKey.String("kitex"),
					semconv.RPCService("risk-manage"),
					semconv.RPCMethod(method),
				),
			)
			if caller != "" {
				span.SetAttributes(attribute.String("rpc.caller.service", caller))
			}
			defer span.End()

			err := next(ctx, req, resp)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				span.SetStatus(codes.Ok, "")
			}
			return err
		}
	}
}

// injectTraceContext 出站调用用的辅助 (目前 risk-manage 没有出站 Kitex 调用所以
// 不挂 client MW, 但保留这个函数让未来 audit kafka producer / 下游 Kitex client
// 接入时一行就能用).
//
// 用法: ctx = injectTraceContext(ctx); 后续 RPC 调用 → traceparent 自动透传.
func injectTraceContext(ctx context.Context) context.Context {
	propagator := otel.GetTextMapPropagator()
	ctxPtr := ctx
	propagator.Inject(ctx, metainfoCarrier{ctx: &ctxPtr})
	return ctxPtr
}

// 静态确认 metainfoCarrier 实现 propagation.TextMapCarrier — 编译期把契约固定住,
// 改 metainfo API 时编译就报错.
var _ propagation.TextMapCarrier = metainfoCarrier{}

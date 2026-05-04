package logging

// gRPC 分层日志拦截器
//
// 每条 gRPC 请求经过 UnaryServerInterceptor 时：
//   - 生成 trace_id 并放入 ctx + 写入响应头，便于客户端关联
//   - 在 api.log 记录：trace_id、方法名、请求体（JSON）、响应体（JSON）、状态码、错误信息
//   - 在 performance.log 记录：trace_id、方法名、耗时（ms）、是否出错
//
// 下游 service / repository 通过 logging.LoggerFromCtx(ctx, baseLogger) 拿到带 trace_id
// 的 logger，所有结构化日志都会自动带 trace_id 字段，可端到端关联一条请求。
//
// 请求/响应 JSON 采用 proto message 的 JSON 序列化；若序列化失败则写 "<marshal error>"。
// 超大请求/响应会被截断到 maxLogBodyBytes，防止日志爆量。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const maxLogBodyBytes = 4096 // 单条请求/响应 JSON 最大记录字节数

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
// 建议 service / repository 在每个 ctx-aware 方法的入口调用一次，缓存到本地变量后
// 调下游所有 zap.Info/Warn/Error，保证整条请求链路日志带同一个 trace_id。
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

// newTraceID 生成 16-hex-char (8-byte) trace id。短到日志友好，碰撞率对单进程足够低。
func newTraceID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 极端情况退化为时间戳；rand.Read 返回错误几乎不可能（但完整性兜底）
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// extractIncomingTraceID 优先用客户端 / 上游网关传入的 x-trace-id，否则新建一个。
// 让一条用户请求的 trace_id 跨多个微服务保持一致。
func extractIncomingTraceID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if ok {
		if vals := md.Get(TraceIDHeader); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return newTraceID()
}

// NewUnaryServerInterceptor 创建记录请求/响应与耗时的 gRPC 一元拦截器。
//
//	apiLogger  — 接收请求/响应详情（写入 api.log）
//	perfLogger — 接收 API 耗时指标（写入 performance.log）
//
// 同时负责 trace_id 生命周期：复用上游 x-trace-id 或自己生成；注入 ctx；
// 写入响应 header；日志条目都带 trace_id 字段，可端到端关联一条请求。
func NewUnaryServerInterceptor(apiLogger, perfLogger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		start := time.Now()

		// trace_id：优先用上游传来的 x-trace-id，没有则新建
		traceID := extractIncomingTraceID(ctx)
		ctx = WithTraceID(ctx, traceID)
		// 写到响应 header，方便客户端在自己的日志里把这个 trace_id 关联起来
		_ = grpc.SetHeader(ctx, metadata.Pairs(TraceIDHeader, traceID))

		// 记录请求体
		reqBody := marshalBody(req)
		apiLogger.Info("gRPC request received",
			zap.String("trace_id", traceID),
			zap.String("method", info.FullMethod),
			zap.String("request", reqBody),
		)

		// 执行实际处理器
		resp, err := handler(ctx, req)

		durationMs := float64(time.Since(start).Microseconds()) / 1000.0
		code := codes.OK
		var errMsg string
		if err != nil {
			st, _ := status.FromError(err)
			code = st.Code()
			errMsg = st.Message()
		}

		// 记录响应体到 API 日志
		apiLogger.Info("gRPC request completed",
			zap.String("trace_id", traceID),
			zap.String("method", info.FullMethod),
			zap.String("response", marshalBody(resp)),
			zap.String("code", code.String()),
			zap.String("error", errMsg),
			zap.Float64("duration_ms", durationMs),
		)

		// 记录耗时到性能日志
		perfLogger.Info("api",
			zap.String("trace_id", traceID),
			zap.String("method", info.FullMethod),
			zap.Float64("duration_ms", durationMs),
			zap.String("code", code.String()),
			zap.Bool("error", err != nil),
		)

		return resp, err
	}
}

// marshalBody 将请求/响应序列化为 JSON 字符串（优先 protojson，回退 encoding/json）。
// 超过 maxLogBodyBytes 时截断并附加 "...[truncated]"。
func marshalBody(v interface{}) string {
	if v == nil {
		return "null"
	}
	var b []byte
	var err error
	if msg, ok := v.(proto.Message); ok {
		b, err = protojson.Marshal(msg)
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	if len(b) > maxLogBodyBytes {
		return string(b[:maxLogBodyBytes]) + "...[truncated]"
	}
	return string(b)
}

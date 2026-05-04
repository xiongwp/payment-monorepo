// Package shadow 提供影子流量（压测）标识的 context 传递、gRPC interceptor、
// 以及表名后缀工具。
//
// 设计契约（跨服务统一）：
//
//   - gRPC metadata key:  "x-shadow"
//   - 值:                 "1" / "true" / "on" → 影子；其他 → 主流量
//   - context key:        包内 unexported type（防止误用），通过
//                         WithShadow / IsShadow 读写
//   - 落库:               shadow=true 的请求所有 SQL 表后缀加 "_shadow"，
//                         例如 payment_intent_42 → payment_intent_42_shadow
//   - Redis key / Kafka topic: 同样规则（本包暂未使用，留给未来 payment-channel）
//
// 安全准则：
//   - 任何 outbound RPC 必须透传 shadow 标识，否则下游会把压测当生产；
//     UnaryClientInterceptor 自动做了这件事。
//   - 任何 SQL 路径必须经过 sharding.Router.TableName(ctx, …) 而不是
//     GetTableName(…)；后者已被标记为 deprecated。
//   - 鉴权层不对 shadow 流量放宽，但建议接入鉴权时拒绝来自非可信来源的
//     x-shadow header（生产环境 IDC 网关会带上）。
package shadow

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// MetadataKey gRPC metadata 里携带 shadow 标识的 key。
const MetadataKey = "x-shadow"

// Suffix 影子表 / 影子 key 的后缀。改这个常量需要同步生产数据库 ALTER。
const Suffix = "_shadow"

// shadowCtxKey 私有 context key，避免外部直接 ctx.Value 误用。
type shadowCtxKey struct{}

// WithShadow 在 ctx 上挂 shadow=on/off。on=true 时所有下游 SQL / RPC 走影子路径。
func WithShadow(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, shadowCtxKey{}, on)
}

// IsShadow 读 ctx 里的 shadow flag；未设置默认 false（主流量）。
func IsShadow(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(shadowCtxKey{}).(bool)
	return v
}

// TableName 给定主表名，按 ctx 决定是否加影子后缀。
// shadow → "<base>_shadow"；主流量 → 原名。
func TableName(ctx context.Context, base string) string {
	if IsShadow(ctx) {
		return base + Suffix
	}
	return base
}

// FromMetadata 从 incoming gRPC metadata 解析 shadow flag。
// 接受 "1" / "true" / "on"（大小写不敏感）。
func FromMetadata(md metadata.MD) bool {
	if md == nil {
		return false
	}
	for _, v := range md.Get(MetadataKey) {
		v = strings.TrimSpace(strings.ToLower(v))
		if v == "1" || v == "true" || v == "on" {
			return true
		}
	}
	return false
}

// UnaryServerInterceptor 把 incoming metadata["x-shadow"] 写到 ctx，
// 之后所有 service / repo 调用通过 IsShadow(ctx) 决策。
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok && FromMetadata(md) {
			ctx = WithShadow(ctx, true)
		}
		return handler(ctx, req)
	}
}

// UnaryClientInterceptor 把 ctx 里的 shadow flag 写进 outbound metadata，
// 让下游服务（payment-core / accounting / payment-channel）看到同样的标识。
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if IsShadow(ctx) {
			ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, "1")
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

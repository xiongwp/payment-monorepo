// Package shadow 提供"影子流量"（压测）标识在跨服务调用链上的传递、gRPC
// interceptor、以及表名 / Redis key / Kafka topic 后缀工具。
//
// 设计契约（全平台统一）：
//
//	gRPC metadata key:   "x-shadow"
//	可识别真值:           "1" / "true" / "on"（大小写不敏感）
//	context key:         本包 unexported type，外部用 WithShadow / IsShadow 读写
//	后缀:                "_shadow"，统一应用到
//	                       SQL 表名:    payment_intent_42 → payment_intent_42_shadow
//	                       Redis key:   balance:acct123  → balance:acct123_shadow
//	                       Kafka topic: ledger.entry     → ledger.entry_shadow
//
// 平台规则：
//   - 任何持久化路径（SQL / Redis / Kafka）必须经过本包提供的 *Name(ctx, base)
//     辅助函数，避免影子流量误写主表
//   - 任何 outbound RPC 必须装 UnaryClientInterceptor 透传 metadata，否则下游
//     服务把压测当生产
//   - 任何接收 RPC 的服务必须装 UnaryServerInterceptor 把 metadata 翻进 ctx
//   - 与外部 API（商户 webhook / 渠道 GCash 等）交互的代码必须在入口检查
//     IsShadow(ctx) 并直接 short-circuit；shadow 流量绝不外发真实请求
//   - 风控类决策服务（risk-manage）必须在 RPC handler 入口 short-circuit 放行，
//     不消耗频控配额、不写训练样本、不调外部反欺诈
//
// 安全：
//   - 不要从 ctx 之外的任何来源（如 HTTP query string）解析 shadow flag — 只接
//     受可信入口的 metadata
//   - 鉴权层不对 shadow 流量放宽；生产网关应拒绝来自非可信 IP 的 x-shadow header
package shadow

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// MetadataKey gRPC metadata 里携带 shadow 标识的 key（HTTP header 也用同名）。
const MetadataKey = "x-shadow"

// Suffix 影子表 / 影子 key / 影子 topic 的后缀。改这个常量需要同步生产数据库
// ALTER + Kafka 重建 topic + Redis flushdb；正常情况下永远不要改。
const Suffix = "_shadow"

// shadowCtxKey 私有 context key，避免外部直接 ctx.Value 误用。
type shadowCtxKey struct{}

// WithShadow 在 ctx 上挂 shadow on/off。on=true 时所有下游 SQL / RPC / Redis /
// Kafka 走影子路径。
func WithShadow(ctx context.Context, on bool) context.Context {
	return context.WithValue(ctx, shadowCtxKey{}, on)
}

// IsShadow 读 ctx 里的 shadow flag；未设置默认 false（主流量）。nil ctx 也安全。
func IsShadow(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(shadowCtxKey{}).(bool)
	return v
}

// TableName 给定主表名按 ctx 决定是否加影子后缀：shadow → "<base>_shadow"；
// 主流量 → 原名。所有 SQL repo 都应走此函数生成 Table(...) 入参。
func TableName(ctx context.Context, base string) string {
	if IsShadow(ctx) {
		return base + Suffix
	}
	return base
}

// RedisKey 给定主 key 按 ctx 决定是否加影子后缀。和 TableName 同语义。
//
// 注意：因为 Suffix 在末尾，前缀扫描（KEYS / SCAN with MATCH "balance:*"）依然
// 能扫到影子 key；如需严格隔离请用前缀方式（自行包装）。
func RedisKey(ctx context.Context, base string) string {
	if IsShadow(ctx) {
		return base + Suffix
	}
	return base
}

// KafkaTopic 给定主 topic 按 ctx 决定是否加影子后缀。和 TableName 同语义。
// Producer 在每次发送前调用；Consumer 启动时把主 + 影子两个 topic 都订阅起来，
// 内部用 IsShadow(record.headers) 决定如何处理。
func KafkaTopic(ctx context.Context, base string) string {
	if IsShadow(ctx) {
		return base + Suffix
	}
	return base
}

// FromMetadata 从 incoming gRPC metadata（或显式构造的 metadata.MD）解析 shadow
// flag。接受 "1" / "true" / "on"（大小写不敏感、首尾空白）。
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

// UnaryServerInterceptor 把 incoming metadata["x-shadow"] 写到 ctx，之后所有
// service / repo 调用通过 IsShadow(ctx) 决策。
//
// 装在 gRPC server 的 ChainUnaryInterceptor 里，建议在 trace interceptor 之后
// 立即装；这样 access log / metric 都能拿到 shadow 维度。
func UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if md, ok := metadata.FromIncomingContext(ctx); ok && FromMetadata(md) {
			ctx = WithShadow(ctx, true)
		}
		return handler(ctx, req)
	}
}

// UnaryClientInterceptor 把 ctx 里的 shadow flag 写进 outbound metadata，让下游
// 服务（payment-core / accounting / payment-channel / risk-manage / ...）看到
// 同样的标识。
//
// 装在 gRPC client 的 WithChainUnaryInterceptor 里，与 trace 客户端 interceptor
// 并列。
func UnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if IsShadow(ctx) {
			ctx = metadata.AppendToOutgoingContext(ctx, MetadataKey, "1")
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// HTTPHeaderToContext 给 HTTP 网关侧用：从 http.Request.Header 取 x-shadow，
// 写进 ctx 后传给下游 gRPC 客户端。
//
// 网关应只信任来自可信 IP 段（IDC 内网 / 压测平台）的 x-shadow header；公网
// 入口不要直接透传到 ctx。
func HTTPHeaderToContext(ctx context.Context, header HeaderGetter) context.Context {
	if header == nil {
		return ctx
	}
	v := strings.TrimSpace(strings.ToLower(header.Get(MetadataKey)))
	if v == "1" || v == "true" || v == "on" {
		return WithShadow(ctx, true)
	}
	return ctx
}

// HeaderGetter 是 *http.Header 的最小接口；接受 *http.Header 或自定义 mock。
// 不直接 import net/http 避免 payment-util 强依赖。
type HeaderGetter interface {
	Get(key string) string
}

// WithoutCancel 是 context.WithoutCancel 的兼容垫片（Go 1.21+ 已内置）。
// 用于在 spawn 后台 goroutine 时保留 ctx 里的 shadow flag 但脱离调用方 cancel。
//
// 例如：webhook dispatcher 在 Enqueue 返回后异步投递，调用方 ctx 可能 canceled，
// 异步 goroutine 用 WithoutCancel 拿到一个不带 deadline 的 ctx 但保留 shadow flag。
//
// 调用方有 Go ≥ 1.21 时建议直接用 context.WithoutCancel；本函数等价。
func WithoutCancel(parent context.Context) context.Context {
	return detachedContext{parent: parent}
}

type detachedContext struct{ parent context.Context }

func (detachedContext) Deadline() (time.Time, bool)         { return time.Time{}, false }
func (detachedContext) Done() <-chan struct{}               { return nil }
func (detachedContext) Err() error                          { return nil }
func (d detachedContext) Value(key interface{}) interface{} { return d.parent.Value(key) }

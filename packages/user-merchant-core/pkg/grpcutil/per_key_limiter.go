package grpcutil

import (
	"context"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// PerKeyLimitOptions 按调用方标识维度的令牌桶限流。
//
// KeyFn: 从 ctx/metadata 里提取一个稳定 key（如 merchant_id、api key hash、IP）。
//        返回 "" 表示跳过限流。
// RPS/Burst: 每个 key 单独的桶参数。
// Capacity: LRU 上限；超过就淘汰最旧的 key 的桶（对应调用方下一次请求会重置）。
// TTL: 空闲多久后的桶自动清掉，防止 key 爆炸。
type PerKeyLimitOptions struct {
	KeyFn    func(ctx context.Context, fullMethod string) string
	RPS      float64
	Burst    int
	Capacity int
	TTL      time.Duration
}

// PerKeyRateLimitInterceptor 每个 key 独立桶；KeyFn 为 nil 或 RPS<=0 退化为 no-op。
// 典型配置：key=merchant_id, rps=100, burst=200 —— 某个商户狂刷不影响其它商户。
func PerKeyRateLimitInterceptor(opt PerKeyLimitOptions) grpc.UnaryServerInterceptor {
	if opt.KeyFn == nil || opt.RPS <= 0 {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	if opt.Burst <= 0 {
		opt.Burst = int(opt.RPS)
		if opt.Burst < 1 {
			opt.Burst = 1
		}
	}
	if opt.Capacity <= 0 {
		opt.Capacity = 10_000
	}
	if opt.TTL <= 0 {
		opt.TTL = 10 * time.Minute
	}
	buckets := expirable.NewLRU[string, *rate.Limiter](opt.Capacity, nil, opt.TTL)
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		key := opt.KeyFn(ctx, info.FullMethod)
		if key == "" {
			return handler(ctx, req)
		}
		lim, ok := buckets.Get(key)
		if !ok {
			lim = rate.NewLimiter(rate.Limit(opt.RPS), opt.Burst)
			buckets.Add(key, lim)
		}
		if !lim.Allow() {
			return nil, status.Errorf(codes.ResourceExhausted, "per-key rate limit exceeded: %s", key)
		}
		return handler(ctx, req)
	}
}

// KeyFromMetadata 从 gRPC metadata 中提取 key（比如 "x-merchant-id" header）。空值返回 ""。
func KeyFromMetadata(header string) func(ctx context.Context, _ string) string {
	return func(ctx context.Context, _ string) string {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return ""
		}
		vals := md.Get(header)
		if len(vals) == 0 {
			return ""
		}
		return vals[0]
	}
}

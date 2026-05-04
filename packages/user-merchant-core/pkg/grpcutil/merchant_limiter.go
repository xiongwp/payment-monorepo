package grpcutil

import (
	"context"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MerchantLimitResolver 返回某 merchant 当前的 rps 上限 + 该请求应归属的 key。
// key 为空时跳过限流；rps<=0 时使用 DefaultRPS。
type MerchantLimitResolver func(ctx context.Context, fullMethod string) (key string, rps float64)

// MerchantRateLimitOptions 按商户粒度的自适应限流参数。
//
// 与 PerKeyRateLimit 的区别：每个 key 的 RPS 不是全局配置，而是来自 Resolver
// （通常从缓存里取 merchant.rate_limit_rps）。商户升级/降级后自动生效。
type MerchantRateLimitOptions struct {
	Resolver   MerchantLimitResolver
	DefaultRPS float64       // resolver 返回 0/负值时的兜底；<=0 直接跳过
	Capacity   int           // LRU 上限
	TTL        time.Duration // 空闲 TTL
}

type bucket struct {
	lim *rate.Limiter
	rps float64 // 上次构造时的 rps，用于检测配置变化重建
}

// MerchantRateLimitInterceptor 每商户独立令牌桶；resolver 返回的 key 决定桶归属。
// resolver 返回的 rps 变化时自动重建桶（商户提升/降级不需要重启服务）。
func MerchantRateLimitInterceptor(opt MerchantRateLimitOptions) grpc.UnaryServerInterceptor {
	if opt.Resolver == nil {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	if opt.Capacity <= 0 {
		opt.Capacity = 10_000
	}
	if opt.TTL <= 0 {
		opt.TTL = 10 * time.Minute
	}
	buckets := expirable.NewLRU[string, *bucket](opt.Capacity, nil, opt.TTL)
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		key, rps := opt.Resolver(ctx, info.FullMethod)
		if key == "" {
			return handler(ctx, req)
		}
		if rps <= 0 {
			rps = opt.DefaultRPS
		}
		if rps <= 0 {
			// 没有可用 rps → 直接放行（保持可用性优先于严格限流；真正要卡死应给 DefaultRPS）
			return handler(ctx, req)
		}
		b, ok := buckets.Get(key)
		if !ok || b.rps != rps {
			// 新桶，或配置变了：按新 rps 建。burst 直接 = rps（允许 1s 突发，避免常态抖动）
			burst := int(rps)
			if burst < 1 {
				burst = 1
			}
			b = &bucket{lim: rate.NewLimiter(rate.Limit(rps), burst), rps: rps}
			buckets.Add(key, b)
		}
		if !b.lim.Allow() {
			return nil, status.Errorf(codes.ResourceExhausted,
				"merchant %s rate limit exceeded (rps=%g)", key, rps)
		}
		return handler(ctx, req)
	}
}

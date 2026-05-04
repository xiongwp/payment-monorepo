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

// LimiterBackend 单机/分布式限流后端的抽象。允许把当前的进程内 LRU 实现替换成
// Redis/memcached/一致性哈希环等分布式方案，而不改拦截器逻辑。
//
// Allow 返回 true = 放行、false = 拒绝。实现必须是并发安全的，并且对相同 (key, rps)
// 在 1s 窗口内允许 ~rps 次请求。
type LimiterBackend interface {
	Allow(ctx context.Context, key string, rps float64, burst int) (bool, error)
}

// PerKeyRateLimitInterceptorWith 允许注入 LimiterBackend 版本。
// 把之前的 PerKeyRateLimitInterceptor 逻辑转到可插拔后端上；
// 默认（backend==nil）退化到 in-process LRU 老实现（本文件里的 inProcessBackend）。
//
// 分布式场景（Redis）：注入一个调 lua 脚本（token-bucket / sliding-window）的实现。
func PerKeyRateLimitInterceptorWith(opt PerKeyLimitOptions, backend LimiterBackend) grpc.UnaryServerInterceptor {
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
	if backend == nil {
		backend = newInProcessBackend(opt.Capacity, opt.TTL)
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		key := opt.KeyFn(ctx, info.FullMethod)
		if key == "" {
			return handler(ctx, req)
		}
		allowed, err := backend.Allow(ctx, key, opt.RPS, opt.Burst)
		if err != nil {
			// 后端故障（Redis 断）时 fail-open：不卡业务，走没限流状态。
			// 真正希望 fail-close 的场景在 backend 实现里加开关。
			return handler(ctx, req)
		}
		if !allowed {
			return nil, status.Errorf(codes.ResourceExhausted,
				"per-key rate limit exceeded: %s", key)
		}
		return handler(ctx, req)
	}
}

// inProcessBackend 默认实现；拷贝自老版本的 LRU 逻辑。
type inProcessBackend struct {
	buckets *expirable.LRU[string, *rate.Limiter]
}

func newInProcessBackend(capacity int, ttl time.Duration) *inProcessBackend {
	if capacity <= 0 {
		capacity = 10_000
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &inProcessBackend{
		buckets: expirable.NewLRU[string, *rate.Limiter](capacity, nil, ttl),
	}
}

func (b *inProcessBackend) Allow(_ context.Context, key string, rps float64, burst int) (bool, error) {
	lim, ok := b.buckets.Get(key)
	if !ok {
		lim = rate.NewLimiter(rate.Limit(rps), burst)
		b.buckets.Add(key, lim)
	}
	return lim.Allow(), nil
}

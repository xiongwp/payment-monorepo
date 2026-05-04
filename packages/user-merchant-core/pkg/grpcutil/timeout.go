package grpcutil

import (
	"context"
	"time"

	"google.golang.org/grpc"
)

// TimeoutInterceptor 为每个 RPC 注入 context timeout —— 让 DB 慢查询 /
// 下游 hang 不会把调用方拖死。优先级：
//   - 每方法单独配置 > 全局 Default
//   - 调用方已经设置了更短的 deadline → 不覆盖（用老的）
// 没有 Default 且没命中 ByMethod 时返回 no-op。
type TimeoutConfig struct {
	Default  time.Duration
	ByMethod map[string]time.Duration
}

func TimeoutInterceptor(cfg TimeoutConfig) grpc.UnaryServerInterceptor {
	if cfg.Default <= 0 && len(cfg.ByMethod) == 0 {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		d := cfg.Default
		if v, ok := cfg.ByMethod[info.FullMethod]; ok {
			d = v
		}
		if d <= 0 {
			return handler(ctx, req)
		}
		// 调用方给的 deadline 更紧就别覆盖
		if existing, ok := ctx.Deadline(); ok && time.Until(existing) <= d {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return handler(ctx, req)
	}
}

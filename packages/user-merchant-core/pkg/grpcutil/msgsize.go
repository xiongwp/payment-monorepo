package grpcutil

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// MsgSizeLimits 按 method 的请求体字节上限。
//   Default：未命中 ByMethod 时的兜底；0 = 不限。
//   ByMethod：fullMethod → max bytes；覆盖 Default。
//
// 比起 grpc.MaxRecvMsgSize 单一值，这里可以给高风险方法 (Create / AddDocument)
// 更宽松、给低风险方法 (Rotate / Transition*) 极紧，缩小 DoS 面。
type MsgSizeLimits struct {
	Default  int
	ByMethod map[string]int
}

// MsgSizeLimitInterceptor 在 handler 前把 req Marshal 一次算体积；
// 超过就立即 ResourceExhausted 返回。非 proto.Message 不校验（不可能发生）。
func MsgSizeLimitInterceptor(limits MsgSizeLimits) grpc.UnaryServerInterceptor {
	if limits.Default <= 0 && len(limits.ByMethod) == 0 {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		limit := limits.Default
		if v, ok := limits.ByMethod[info.FullMethod]; ok {
			limit = v
		}
		if limit <= 0 {
			return handler(ctx, req)
		}
		if m, ok := req.(proto.Message); ok {
			// Marshal 一次是 O(req)，同一笔请求反正要走 gRPC 序列化，不是重复成本。
			if b, err := proto.Marshal(m); err == nil && len(b) > limit {
				return nil, status.Errorf(codes.ResourceExhausted,
					"%s request too large: %dB > limit %dB", info.FullMethod, len(b), limit)
			}
		}
		return handler(ctx, req)
	}
}

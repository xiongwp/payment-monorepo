package grpcutil

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/xiongwp/user-merchant-core/pkg/tracex"
)

// ClientDialOptions 拨号参数。
type ClientDialOptions struct {
	// Endpoint: host:port
	Endpoint string
	// Timeout per RPC；<=0 不注入。调用方仍可用 ctx.WithTimeout 自己控制。
	Timeout time.Duration
	// MaxRetries: 0 或 1 = 不重试；仅对 codes.Unavailable / DeadlineExceeded 生效。
	MaxRetries int
	// BearerToken: 非空会加入每个请求的 authorization metadata。
	BearerToken string
	// TraceMetadataKey: 从 outgoing ctx 里提取的 trace id key；不配就不注入。
	// 一般与 tracex.MetadataKey 一致（"x-trace-id"）。
	TraceMetadataKey string
	// Extra: 让调用方自己塞更多 DialOption（比如 TLS）。
	Extra []grpc.DialOption
}

// Dial 统一的 gRPC 客户端构造：
//   - insecure credentials（内部服务）
//   - client 端 keepalive
//   - 自动 bearer 注入
//   - 自动 trace id 透传
//   - 可选的 unary retry
//
// 返回的 *grpc.ClientConn 调用方负责 Close()。
func Dial(opt ClientDialOptions) (*grpc.ClientConn, error) {
	if opt.Endpoint == "" {
		return nil, fmt.Errorf("grpcutil.Dial: endpoint required")
	}
	unary := chainUnary(
		// OTel client span 在最外层：能记录所有重试/超时的全部耗时。
		// 没配 OTel 时 tracer 是 noop，开销可忽略。
		tracex.OTelClientInterceptor(),
		authClientInterceptor(opt.BearerToken),
		traceClientInterceptor(opt.TraceMetadataKey),
		timeoutClientInterceptor(opt.Timeout),
		retryClientInterceptor(opt.MaxRetries),
	)
	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.WithUnaryInterceptor(unary),
	}
	dialOpts = append(dialOpts, opt.Extra...)
	return grpc.NewClient(opt.Endpoint, dialOpts...)
}

// chainUnary 把多个 UnaryClientInterceptor 串联（grpc-go 1.65 未内置这个组合）。
func chainUnary(ins ...grpc.UnaryClientInterceptor) grpc.UnaryClientInterceptor {
	// 过滤 nil
	nonNil := ins[:0]
	for _, i := range ins {
		if i != nil {
			nonNil = append(nonNil, i)
		}
	}
	if len(nonNil) == 0 {
		return nil
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var chain grpc.UnaryInvoker = invoker
		for i := len(nonNil) - 1; i >= 0; i-- {
			cur := nonNil[i]
			next := chain
			chain = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
				return cur(ctx, method, req, reply, cc, next, opts...)
			}
		}
		return chain(ctx, method, req, reply, cc, opts...)
	}
}

func authClientInterceptor(token string) grpc.UnaryClientInterceptor {
	if token == "" {
		return nil
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func traceClientInterceptor(key string) grpc.UnaryClientInterceptor {
	if key == "" {
		return nil
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// 已经在 outgoing metadata 里了就不重复；否则从 incoming md 拾取。
		if md, ok := metadata.FromOutgoingContext(ctx); ok && len(md.Get(key)) > 0 {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get(key); len(vals) > 0 {
				ctx = metadata.AppendToOutgoingContext(ctx, key, vals[0])
			}
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func timeoutClientInterceptor(d time.Duration) grpc.UnaryClientInterceptor {
	if d <= 0 {
		return nil
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if existing, ok := ctx.Deadline(); ok && time.Until(existing) <= d {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// retryClientInterceptor 针对瞬时错误（Unavailable/DeadlineExceeded）做指数退避。
// 只在 max >= 2 时启用。幂等性是调用方的责任 —— 非幂等方法不要走这个 interceptor。
func retryClientInterceptor(max int) grpc.UnaryClientInterceptor {
	if max < 2 {
		return nil
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		var err error
		backoff := 50 * time.Millisecond
		for attempt := 0; attempt < max; attempt++ {
			err = invoker(ctx, method, req, reply, cc, opts...)
			if err == nil {
				return nil
			}
			st, ok := status.FromError(err)
			if !ok {
				return err
			}
			switch st.Code() {
			case codes.Unavailable, codes.DeadlineExceeded:
				// 可重试
			default:
				return err
			}
			if attempt < max-1 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoff):
				}
				backoff *= 2
			}
		}
		return err
	}
}

// WithLoggingClient 可选：让客户端调用也打一行 debug 日志（生产一般关掉）。
func WithLoggingClient(logger *zap.Logger) grpc.UnaryClientInterceptor {
	if logger == nil {
		return nil
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		fields := []zap.Field{
			zap.String("method", method),
			zap.Duration("dur", time.Since(start)),
		}
		if err != nil {
			fields = append(fields, zap.Error(err))
			logger.Warn("grpc client call failed", fields...)
		} else {
			logger.Debug("grpc client call", fields...)
		}
		return err
	}
}

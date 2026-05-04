// Package server gRPC interceptors：recover / log / metrics / rate limit / auth
// 形状刻意和 payment-core 对齐，保证跨仓代码一致。
package server

import (
	"context"
	"crypto/subtle"
	"runtime/debug"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/xiongwp/kms-manage/internal/metrics"
)

func MetricsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		code := codes.OK.String()
		if err != nil {
			if st, ok := status.FromError(err); ok {
				code = st.Code().String()
			} else {
				code = codes.Unknown.String()
			}
		}
		metrics.GRPCRequestTotal.WithLabelValues(info.FullMethod, code).Inc()
		metrics.GRPCRequestDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

// LoggingInterceptor 在每次 RPC 调用进出时打日志：
//   IN  方法名 + 完整 request payload（JSON）
//   OUT 方法名 + 耗时 + status code + 完整 response payload 或错误
//
// 用 protojson 渲染：proto.Message 自动序列化；非 proto 类型（理论上不会在 gRPC 里出现）退回 %+v。
// kms-manage 里 plaintext / ciphertext / DEK 是机密 —— 服务端日志统一脱敏（前 N 字节 + ...）。
func LoggingInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		start := time.Now()
		logger.Info("grpc IN",
			zap.String("method", info.FullMethod),
			zap.String("req", redactKMS(info.FullMethod, req)),
		)
		resp, err := handler(ctx, req)
		dur := time.Since(start)
		if err != nil {
			st, _ := status.FromError(err)
			logger.Warn("grpc OUT",
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", st.Code().String()),
				zap.String("err", err.Error()),
			)
		} else {
			logger.Info("grpc OUT",
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", "OK"),
				zap.String("resp", redactKMS(info.FullMethod, resp)),
			)
		}
		return resp, err
	}
}

// renderProto 把 proto.Message 渲染成单行 JSON；非 proto 退回 fmt。
func renderProto(v interface{}) string {
	if v == nil {
		return "null"
	}
	if m, ok := v.(proto.Message); ok {
		b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
		if err == nil {
			return string(b)
		}
	}
	return "<non-proto>"
}

// redactKMS 截短 plaintext / ciphertext / encrypted_key / plaintext_key 这些机密字段。
// 简单做法：渲染 JSON 后用字符串替换。生产敏感场景应该重写更严谨。
func redactKMS(method string, v interface{}) string {
	s := renderProto(v)
	for _, key := range []string{`"plaintext"`, `"ciphertext"`, `"plaintext_key"`, `"encrypted_key"`} {
		s = redactJSONField(s, key)
	}
	return s
}

// redactJSONField 把 "key":"long..value" 截成 "key":"<len:N>"
func redactJSONField(s, key string) string {
	for {
		i := strings.Index(s, key+`:"`)
		if i < 0 {
			return s
		}
		start := i + len(key) + 2 // skip key":"
		end := strings.Index(s[start:], `"`)
		if end < 0 {
			return s
		}
		end += start
		s = s[:i] + key + `:"<len:` + intStr(end-start) + `>"` + s[end+1:]
	}
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// AuthInterceptor 校验 Bearer token。validTokens 的 key 是合法 token，value 是
// 标签（如 caller name，仅用于日志/审计；当前实现未读取）。
//
// 安全：用 subtle.ConstantTimeCompare 逐 token 比较防 timing attack。原先用
// map[token]→ok 直接 lookup 看似 O(1) 但 byte 比较行为受 hash 桶 + 长度差异
// 影响，泄露信息。改为统一 O(N) 全扫，N 是 valid token 个数（典型 < 10）。
//
// 不在错误 / 日志里回显 token 长度或前缀。
func AuthInterceptor(validTokens map[string]string, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	// 启动期把 map key 物化成 [][]byte，避免每条请求重新 range map（map iteration
	// 顺序随机，理论上也算微弱 timing 信号）。
	expected := make([][]byte, 0, len(validTokens))
	for k := range validTokens {
		expected = append(expected, []byte(k))
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if len(expected) == 0 || skip(info.FullMethod) {
			return handler(ctx, req)
		}
		md, _ := metadata.FromIncomingContext(ctx)
		auth := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
		if !strings.HasPrefix(auth, "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		tok := []byte(strings.TrimPrefix(auth, "Bearer "))
		// OR 累加全扫 → 任一匹配 = match==1。不要 break early，避免泄露"哪个槽位
		// 命中"的 timing 信号。
		var match int
		for _, e := range expected {
			match |= subtle.ConstantTimeCompare(tok, e)
		}
		if match != 1 {
			// 仅记 method（不带 token / token 长度），方便排查"哪个 caller 配错了"。
			logger.Warn("auth rejected", zap.String("method", info.FullMethod))
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		return handler(ctx, req)
	}
}

func RateLimitInterceptor(rps float64, burst int) grpc.UnaryServerInterceptor {
	if rps <= 0 {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
	if burst <= 0 {
		burst = int(rps)
		if burst < 1 {
			burst = 1
		}
	}
	limiter := rate.NewLimiter(rate.Limit(rps), burst)
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if !limiter.Allow() {
			return nil, status.Error(codes.ResourceExhausted, "rate limit exceeded")
		}
		return handler(ctx, req)
	}
}

func RecoverInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic in grpc handler",
					zap.String("method", info.FullMethod),
					zap.Any("recover", r),
					zap.String("stack", string(debug.Stack())))
				err = status.Errorf(codes.Internal, "internal panic")
			}
		}()
		return handler(ctx, req)
	}
}

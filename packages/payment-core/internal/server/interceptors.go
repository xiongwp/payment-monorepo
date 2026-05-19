// Package server gRPC interceptors：recover / log / metrics / rate limit / auth
package server

import (
	"context"
	"crypto/subtle"
	"runtime/debug"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/xiongwp/payment-core/internal/metrics"
	"github.com/xiongwp/payment-util/piiredact" // ROI-1: PII 脱敏
	"github.com/xiongwp/payment-util/ratelimit"
)

// LoggingInterceptor 每次 RPC 进出都打日志：
//   IN  方法 + 完整 request payload（protojson）
//   OUT 方法 + 耗时 + status code + 完整 response payload 或 error
//
// 用 zap.Info 级别，prod / dev 两种 logger 都会输出（只是 prod 是 JSON 单行、
// dev 是人读多行）—— 排查问题时永远有迹可循。
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
			zap.String("req", renderProto(req)),
		)
		resp, err := handler(ctx, req)
		dur := time.Since(start)
		if err != nil {
			st, _ := status.FromError(err)
			logger.Warn("grpc OUT",
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", st.Code().String()),
				zap.String("err", err.Error()))
		} else {
			logger.Info("grpc OUT",
				zap.String("method", info.FullMethod),
				zap.Duration("dur", dur),
				zap.String("code", "OK"),
				zap.String("resp", renderProto(resp)))
		}
		return resp, err
	}
}

// renderProto 把 proto.Message 渲染为 JSON 字符串 (用于日志).
//
// ROI-1: 用 piiredact 做 Luhn 扫描 — 抓走任何 13-19 位 Luhn-valid 数字串 (card_number / pan
// 等), 输出 BIN+last4 占位. 这是 payment-core 日志泄漏卡号的主要兜底.
//
// 注: 完整的字段名脱敏 (email/phone/token 等) 需要 unmarshal-then-redact-then-remarshal,
// 当前为了简化只做 Luhn 扫描; 字段名脱敏走 piiredact.ZapField 在 ad-hoc 调用点接.
func renderProto(v interface{}) string {
	if v == nil {
		return "null"
	}
	if m, ok := v.(proto.Message); ok {
		b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
		if err == nil {
			return piiredact.RedactString(string(b))
		}
	}
	return "<non-proto>"
}

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

// AuthInterceptor 默认拒绝模式：
//   - validTokens 非空 → 校验 Bearer token；未带 / 不匹配 → Unauthenticated
//   - validTokens 为空 + allowUnauthenticated=true → 放行（dev / lab 显式打开）
//   - validTokens 为空 + allowUnauthenticated=false（生产默认）→ 启动期 fail-closed：
//     拒绝所有非健康检查 / reflection 请求
//
// 与 order-core 同步签名（P1-1）：避免某次部署忘配 token 时被静默放过去。
//
// timing-safe：用 subtle.ConstantTimeCompare 对 token 做常量时间比较，
// 防止外部根据返回延迟探测 token 长度 / 前缀。
func AuthInterceptor(validTokens map[string]string, allowUnauthenticated bool, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	// 预计算 expected []byte 列表，避免热路径 map 遍历 + string→[]byte 重复转换。
	expected := make([][]byte, 0, len(validTokens))
	for tok := range validTokens {
		expected = append(expected, []byte(tok))
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		if len(validTokens) == 0 {
			if allowUnauthenticated {
				return handler(ctx, req)
			}
			logger.Warn("AuthInterceptor: no tokens configured and allowUnauthenticated=false; rejecting",
				zap.String("method", info.FullMethod))
			return nil, fmt.Errorf("auth not configured")
		}
		md, _ := /* TODO Kitex metainfo */ (interface{}, bool)(nil, false) /* was: metadata.FromIncomingContext(ctx) */
		auth := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
		if !strings.HasPrefix(auth, "Bearer ") {
			return nil, fmt.Errorf("missing bearer token")
		}
		tok := []byte(strings.TrimPrefix(auth, "Bearer "))
		var match int
		for _, e := range expected {
			match |= subtle.ConstantTimeCompare(tok, e)
		}
		if match != 1 {
			logger.Debug("auth rejected", zap.String("method", info.FullMethod))
			return nil, fmt.Errorf("invalid token")
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
			return nil, fmt.Errorf("rate limit exceeded")
		}
		return handler(ctx, req)
	}
}

// MerchantRateLimitInterceptor merchant 维度限流（从 metadata X-Merchant-ID 提取）。
// 对应 gRPC metadata["x-merchant-id"]；没有 → 不限流（IP 限流兜底）。
//
// 限流参数由 limiter 管理（支持热更新）；当触发限流时记指标 + 返 ResourceExhausted。
func MerchantRateLimitInterceptor(limiter *ratelimit.MerchantLimiter, logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		md, _ := /* TODO Kitex metainfo */ (interface{}, bool)(nil, false) /* was: metadata.FromIncomingContext(ctx) */
		merchantID := strings.TrimSpace(strings.Join(md.Get("x-merchant-id"), ""))
		if merchantID != "" && !limiter.Allow(merchantID) {
			if logger != nil {
				logger.Warn("merchant rate limit exceeded",
					zap.String("merchant_id", merchantID),
					zap.String("method", info.FullMethod))
			}
			return nil, fmt.Errorf("merchant rate limit exceeded")
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
				err = fmt.Errorf("internal panic")
			}
		}()
		return handler(ctx, req)
	}
}

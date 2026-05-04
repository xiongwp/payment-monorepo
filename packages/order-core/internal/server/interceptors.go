// Package server gRPC interceptor：log / metrics / auth / rate limit
package server

import (
	"context"
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

	"github.com/xiongwp/order-core/internal/metrics"
)

// LoggingInterceptor 每次 RPC 进出都打日志：方法 + 完整 req/resp（protojson）+ 耗时。
// Info 级别 —— prod / dev logger 都会输出，prod JSON / dev 人读。
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
			zap.String("req", redactSensitive(renderProto(req))),
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
				zap.String("resp", redactSensitive(renderProto(resp))))
		}
		return resp, err
	}
}

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

// sensitiveLogKeys 是 redactSensitive 的全局敏感字段名单。
//
// **P2-5 维护说明**：手动列表容易遗漏（业务侧加新字段不会同步）。长期方案是：
//   1) struct tag 标记（`sensitive:"true"`），用反射遍历自动脱敏；
//   2) 或换 zap 自定义 Encoder，提供 zap.Sensitive() 类型 + 受控编码；
//   3) 或在 proto 层用 google.protobuf.FieldOption + custom plugin 在 .proto 文件
//      标 [(sensitive) = true]，protojson marshal 时跳过。
//
// 切换前的兜底是把已知的字段名集中维护，新加敏感字段时同步更新本切片。
// 命名约定：camelCase 和 snake_case 都列（protojson 可能两种之一）。
var sensitiveLogKeys = []string{
	// 凭证 / token
	`"clientSecret"`, `"client_secret"`,
	`"webhookSecret"`, `"webhook_secret"`,
	`"liveKey"`, `"live_key"`, `"testKey"`, `"test_key"`,
	`"apiKey"`, `"api_key"`,
	`"accessToken"`, `"access_token"`, `"refreshToken"`, `"refresh_token"`,
	`"verificationToken"`, `"verification_token"`,
	// 渠道密钥
	`"acquirerSecret"`, `"acquirer_secret"`,
	`"partnerSecret"`, `"partner_secret"`,
	// 卡 / 银行账户
	`"cardNumber"`, `"card_number"`, `"pan"`,
	`"cvv"`, `"cvc"`, `"cvv2"`,
	`"iban"`, `"accountNumber"`, `"account_number"`,
	`"settlementAccount"`, `"settlement_account"`,
	// 用户认证
	`"password"`, `"otp"`, `"pin"`,
	`"totpSecret"`, `"totp_secret"`,
	// PII
	`"taxId"`, `"tax_id"`,
}

// redactSensitive 把 JSON 日志里的敏感字段值截短，防止 client_secret / token / key 入磁盘。
//
// 受 sensitiveLogKeys 名单驱动；新增敏感字段必须同步更新名单。
func redactSensitive(s string) string {
	for _, key := range sensitiveLogKeys {
		s = redactField(s, key)
	}
	return s
}

func redactField(s, key string) string {
	for {
		idx := strings.Index(s, key+`:"`)
		if idx < 0 {
			idx = strings.Index(s, key+`": "`)
			if idx < 0 {
				return s
			}
		}
		// find start of value
		start := strings.Index(s[idx:], `"`)
		if start < 0 {
			return s
		}
		// skip key quote
		valStart := idx + start + 1
		valStart2 := strings.IndexByte(s[valStart:], '"')
		if valStart2 < 0 {
			return s
		}
		valStart += valStart2 + 1
		valEnd := strings.IndexByte(s[valStart:], '"')
		if valEnd < 0 {
			return s
		}
		valEnd += valStart
		s = s[:valStart] + "***REDACTED***" + s[valEnd:]
	}
}

// MetricsInterceptor 记录每个 gRPC 请求的计数 + 耗时
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

// AuthInterceptor 简单的 bearer token 鉴权
//
// - 读取 metadata["authorization"]，期望 "Bearer <token>" 格式
// - token 必须等于 validTokens 里的某个才放行
// - /grpc.health.v1/* 和 /grpc.reflection.v1alpha.* 不拦
// - 安全默认：tokens 空 → 一律拒绝（防止配置漏注入导致全 RPC 裸奔）
// - dev / 测试场景需显式传 allowUnauthenticated=true 才放行
//
// 真实部署请替换成 JWT 校验 / mTLS / IAM；此处只做最小可用。
func AuthInterceptor(validTokens map[string]string, allowUnauthenticated bool, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	if allowUnauthenticated && logger != nil {
		logger.Warn("AuthInterceptor: allow_unauthenticated=true — all RPCs accepted without token. DO NOT USE IN PRODUCTION.")
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		// 安全默认：没有配置 token 又没显式允许匿名 → 拒绝（双保险，启动期也会 fail）
		if len(validTokens) == 0 {
			if allowUnauthenticated {
				return handler(ctx, req)
			}
			logger.Warn("auth rejected: no valid tokens configured",
				zap.String("method", info.FullMethod))
			return nil, status.Error(codes.Unauthenticated, "auth not configured")
		}
		md, _ := metadata.FromIncomingContext(ctx)
		auth := strings.TrimSpace(strings.Join(md.Get("authorization"), ""))
		if !strings.HasPrefix(auth, "Bearer ") {
			return nil, status.Error(codes.Unauthenticated, "missing bearer token")
		}
		tok := strings.TrimPrefix(auth, "Bearer ")
		if _, ok := validTokens[tok]; !ok {
			logger.Debug("auth rejected", zap.String("method", info.FullMethod))
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		return handler(ctx, req)
	}
}

// RateLimitInterceptor 全局 token-bucket 限流（rps + burst）
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

// RecoverInterceptor panic 保护 + 打 stack trace（N1: 便于线上诊断）
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

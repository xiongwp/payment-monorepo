// Package grpcutil 通用 gRPC 拦截器：recover / log / auth / rate-limit。
//
// 不依赖 service 专属 metrics；metrics 由调用方通过 MetricsInterceptor(record)
// 传入 record 回调自己组装。
package grpcutil

import (
	"context"
	"crypto/subtle"
	"os"
	"runtime/debug"
	"strconv"
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
)

// LoggingOptions 参数
type LoggingOptions struct {
	MaxBodyBytes int
	SkipMethods  map[string]struct{}
}

const defaultMaxBodyBytes = 2048

// LoggingInterceptor 详细 trace：方法 + 截断 JSON + 耗时。
func LoggingInterceptor(logger *zap.Logger, opt LoggingOptions) grpc.UnaryServerInterceptor {
	if opt.MaxBodyBytes <= 0 {
		opt.MaxBodyBytes = defaultMaxBodyBytes
	}
	marshaler := protojson.MarshalOptions{EmitUnpopulated: false, UseProtoNames: true}

	skip := func(method string) bool {
		if strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.") {
			return true
		}
		if opt.SkipMethods != nil {
			_, ok := opt.SkipMethods[method]
			return ok
		}
		return false
	}
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if skip(info.FullMethod) {
			return handler(ctx, req)
		}
		start := time.Now()
		logger.Info("grpc IN",
			zap.String("method", info.FullMethod),
			zap.String("req", redactSensitive(renderProto(req, marshaler, opt.MaxBodyBytes))),
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
				zap.String("resp", redactSensitive(renderProto(resp, marshaler, opt.MaxBodyBytes))))
		}
		return resp, err
	}
}

func renderProto(v interface{}, m protojson.MarshalOptions, maxBytes int) string {
	if v == nil {
		return "null"
	}
	pm, ok := v.(proto.Message)
	if !ok {
		return "<non-proto>"
	}
	b, err := m.Marshal(pm)
	if err != nil {
		return "<marshal-err>"
	}
	if len(b) <= maxBytes {
		return string(b)
	}
	return string(b[:maxBytes]) + "…<" + strconv.Itoa(len(b)) + "B total>"
}

// defaultSensitiveKeys 永远 redact 的秘密字段。
var defaultSensitiveKeys = []string{
	`"webhookSecret"`, `"webhook_secret"`,
	`"liveSecretKey"`, `"live_secret_key"`,
	`"testSecretKey"`, `"test_secret_key"`,
	`"apiKey"`, `"api_key"`,
	`"plaintext"`,
	`"password"`, `"otp"`,
	`"client_secret"`, `"clientSecret"`,
	`"card_number"`, `"cardNumber"`, `"cvv"`, `"cvc"`,
}

// piiFieldKeys：PII 默认 mask（保留前 2 + 尾 2），debug 时可以通过
// USERMERCHANTCORE_LOG_PII=1 关闭。mask 能让日志读者判断"是否同一用户"而
// 不泄露原文。
var piiFieldKeys = []string{
	`"contactEmail"`, `"contact_email"`,
	`"contactPhone"`, `"contact_phone"`,
	`"taxId"`, `"tax_id"`,
	`"settleAccount"`, `"settle_account"`,
	`"settleHolder"`, `"settle_holder"`,
}

// piiEnabled 启动时一次 env 判断；热路径无锁读。
var piiEnabled = os.Getenv("USERMERCHANTCORE_LOG_PII") == "1"

func redactSensitive(s string) string {
	for _, k := range defaultSensitiveKeys {
		s = redactField(s, k)
	}
	if !piiEnabled {
		for _, k := range piiFieldKeys {
			s = maskField(s, k)
		}
	}
	return s
}

// maskField 同 redactField 解析，但替换为 "ab***yz"。
func maskField(s, key string) string {
	for {
		idx := strings.Index(s, key+`:"`)
		if idx < 0 {
			idx = strings.Index(s, key+`": "`)
			if idx < 0 {
				return s
			}
		}
		start := strings.Index(s[idx:], `"`)
		if start < 0 {
			return s
		}
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
		s = s[:valStart] + maskString(s[valStart:valEnd]) + s[valEnd:]
	}
}

func maskString(v string) string {
	if len(v) <= 4 {
		return "***"
	}
	return v[:2] + "***" + v[len(v)-2:]
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
		start := strings.Index(s[idx:], `"`)
		if start < 0 {
			return s
		}
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

// MetricsRecorder 每次 gRPC 完成时被调用；由各 service 传入自己的 Prometheus 桥接。
type MetricsRecorder func(method, code string, durSec float64)

// MetricsInterceptor 记录每个 gRPC 请求的计数 + 耗时；record 为 nil 时退化成 no-op。
func MetricsInterceptor(record MetricsRecorder) grpc.UnaryServerInterceptor {
	if record == nil {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}
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
		record(info.FullMethod, code, time.Since(start).Seconds())
		return resp, err
	}
}

// callerCtxKey 是 AuthInterceptor 鉴权通过后写入 ctx 的 key，
// 后续 audit / 业务逻辑用 CallerFromContext 读取已校验的 caller 标签。
// 不导出 type 防止外部伪造 key 注入假身份。
type callerCtxKey struct{}

// CallerFromContext 返回 AuthInterceptor 鉴权通过后写入的 caller 标签
// （validTokens map 的 value）。空字符串 = 未鉴权（匿名 / health probe / 未启用 auth）。
//
// 用例：audit 拦截器记录 actor；handler 区分 admin-backend / svc-a 调用。
func CallerFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(callerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// AuthInterceptor 简单的 bearer token 鉴权。validTokens 非空才开。
//
// validTokens 的 key 是合法 token，value 是 caller 标签（svc-a / admin-web 等）；
// 鉴权通过后将 label 通过 ctx 传给后续 audit 拦截器（CallerFromContext 读取）。
//
// 安全：用 subtle.ConstantTimeCompare 全扫所有合法 token，OR 累加结果，避免
// 早退泄露命中位置。原先 map[token] 直接 lookup 受 hash 桶 + byte 比较时序影响，
// 理论上泄露 token 长度 / 命中信息。
func AuthInterceptor(validTokens map[string]string, logger *zap.Logger) grpc.UnaryServerInterceptor {
	skip := func(method string) bool {
		return strings.HasPrefix(method, "/grpc.health.") ||
			strings.HasPrefix(method, "/grpc.reflection.")
	}
	type entry struct {
		token []byte
		label string
	}
	expected := make([]entry, 0, len(validTokens))
	for tok, label := range validTokens {
		expected = append(expected, entry{token: []byte(tok), label: label})
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
		// 全扫所有 entry，subtle.ConstantTimeCompare 防 timing attack。
		// 不 break early。匹配命中时记下 label；后续注入 ctx。
		var matchedLabel string
		for _, e := range expected {
			if subtle.ConstantTimeCompare(tok, e.token) == 1 {
				matchedLabel = e.label
			}
		}
		if matchedLabel == "" {
			// 仅记 method（不带 token / 长度 / 前缀），方便排查"哪个 caller 配错"。
			logger.Warn("auth rejected", zap.String("method", info.FullMethod))
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		ctx = context.WithValue(ctx, callerCtxKey{}, matchedLabel)
		return handler(ctx, req)
	}
}

// RateLimitInterceptor 全局 token-bucket 限流。rps <= 0 返回 no-op。
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

// RecoverInterceptor panic 保护 + 打 stack trace
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

// Package grpc: serviceTokenInterceptor 是 accounting-system 入口的服务-到-服务
// 鉴权拦截器。accounting-system 是平台资金真理源，必须防止内网任何进程拿到 IP
// 后就能直接读写账户。设计要点：
//
//   - 共享 secret token（来自 ACCOUNTING_SERVICE_TOKEN env 或 config）
//     调用方在 metadata 加 `x-svc-token: <token>`；服务端 constant-time 比较。
//   - token 为空时退化为 warn-only：拦截器仍挂着、但请求放行；启动时打一次
//     大写 WARN 提示生产应配置。这样 dev / fresh-rebuild 不会 break，但 ops
//     不会忘了配。
//   - 健康检查 + reflection 走豁免（K8s probe + grpcurl 调试场景不带 metadata）。
//
// 客户端用法（outgoing context）：
//
//	ctx = metadata.AppendToOutgoingContext(ctx, grpcserver.MetadataServiceTokenKey, token)
package grpc

import (
	"context"
	"crypto/subtle"
	"strings"

	"go.uber.org/zap"
)

// MetadataServiceTokenKey 是调用方传递服务 token 的 metadata 头名（小写）。
// gRPC metadata 大小写不敏感，但小写是规范。
const MetadataServiceTokenKey = "x-svc-token"

// SetServiceToken 由 main.go 在 ListenAndServe 之前调用。空字符串 = 退到
// warn-only 模式。
func (s *Server) SetServiceToken(token string) {
	s.serviceToken = token
}

// serviceTokenInterceptor 构造一个 UnaryServerInterceptor。
//
// token == "" 时进入 warn-only 模式：返回的 interceptor 不做校验直接放行；调
// 用方启动时已经打了 WARN，无需每条请求重复 log。
//
// token != "" 时进入严格模式：缺 metadata / 缺 token / 不匹配 一律返回
// Unauthenticated。比较走 subtle.ConstantTimeCompare 防 timing attack。
// 不在错误信息里回显期望长度等任何 token 相关元数据。
func serviceTokenInterceptor(token string, logger *zap.Logger) grpc.UnaryServerInterceptor {
	if token == "" {
		return func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (interface{}, error) {
			return h(ctx, req)
		}
	}
	tokenBytes := []byte(token)
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (interface{}, error) {
		if isAuthExemptMethod(info.FullMethod) {
			return h(ctx, req)
		}
		md, ok := /* TODO Kitex metainfo */ (interface{}, bool)(nil, false) /* was: metadata.FromIncomingContext(ctx) */
		if !ok {
			return nil, fmt.Errorf("missing metadata")
		}
		vals := md.Get(MetadataServiceTokenKey)
		if len(vals) == 0 {
			return nil, fmt.Errorf("missing x-svc-token")
		}
		if subtle.ConstantTimeCompare([]byte(vals[0]), tokenBytes) != 1 {
			// 不打印 token 值。method 信息有助于定位哪个客户端配错了。
			logger.Warn("gRPC: invalid service token",
				zap.String("method", info.FullMethod))
			return nil, fmt.Errorf("invalid x-svc-token")
		}
		return h(ctx, req)
	}
}

// isAuthExemptMethod 返回 true 表示该 gRPC 方法不强制 service token：
//   - /grpc.health.v1.Health/* — K8s gRPC liveness/readiness probe
//   - /grpc.reflection.v1*/*  — grpcurl / 调试客户端
func isAuthExemptMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

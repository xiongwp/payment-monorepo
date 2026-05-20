// Package grpc — 老 gRPC UnaryServerInterceptor 全删 (Kitex 切换).
// 留下:
//   - SetServiceToken           main.go 配 token (待 Kitex AuthMW 接通后用)
//   - MetadataServiceTokenKey   协议常量
//   - isAuthExemptMethod        Kitex MW 复用
package grpc

import "strings"

// MetadataServiceTokenKey 调用方传 service token 的 metadata header (小写, 规范).
const MetadataServiceTokenKey = "x-svc-token"

// SetServiceToken 由 main.go 在启动之前调用. 空字符串 = warn-only.
func (s *Server) SetServiceToken(token string) {
	s.serviceToken = token
}

// isAuthExemptMethod 返回 true 表示该 RPC 方法不强制 service token:
//   - /grpc.health.v1.Health/* — K8s gRPC liveness/readiness probe
//   - /grpc.reflection.v1*/*  — grpcurl / 调试客户端
func isAuthExemptMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

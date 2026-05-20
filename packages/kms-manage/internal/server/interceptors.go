// Package server interceptors — 老 gRPC UnaryServerInterceptor 全删 (Kitex 切换).
//
// 留下的只有 ClientIdentityAllowList 这个数据结构, 由 server/grpc.go 的 Server
// 持有 (用于 mTLS cert CN/SAN 白名单).
//
// Kitex 等价物 (待 kitexutil port 完成后接入 server.WithMiddleware):
//   - RateLimitInterceptor → kitexutil.RateLimitMW(rps, burst)
//   - ClientIdentityInterceptor → kitexutil.SANAllowMW(allowed)
//   - AuthInterceptor → kitexutil.AuthMW(tokens)
//   - LoggingInterceptor → kitexutil.LogMW
//   - MetricsInterceptor → kitexutil.MetricsMW
//   - RecoverInterceptor → kitexutil.RecoverMW
package server

import "strings"

// ClientIdentityAllowList mTLS cert 身份白名单 (CN / SAN DNS / SAN URI 任一命中即放行).
type ClientIdentityAllowList map[string]struct{}

// NewClientIdentityAllowList 构造白名单 (空字符串过滤掉).
func NewClientIdentityAllowList(ids []string) ClientIdentityAllowList {
	out := make(ClientIdentityAllowList, len(ids))
	for _, s := range ids {
		s = strings.TrimSpace(s)
		if s != "" {
			out[s] = struct{}{}
		}
	}
	return out
}

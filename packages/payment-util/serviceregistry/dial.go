// dial.go — DEPRECATED gRPC dial helpers.
//
// 整个 monorepo 切 Kitex 后, gRPC Dial* 系列不再使用. 这里保留空 stub 让
// 已有调用方编译过, 真实运行时会 panic, 强制 caller 切到 Kitex client.
//
// 替代:
//
//	老: conn, err := serviceregistry.DialWithFallback(registry, "svc", addr, opts...)
//	    cli := xxxv1.NewYServiceClient(conn)
//
//	新: cli, err := yservice.NewClient("svc", client.WithHostPorts(addr))
//	    // etcd 服务发现: client.WithResolver(kitexutil.NewEtcdResolver(etcdCli, ""))

package serviceregistry

import (
	"errors"

	"google.golang.org/grpc"
)

// ErrDeprecated — 所有 Dial 函数运行时返回此 error, 强制 caller 切 Kitex.
var ErrDeprecated = errors.New("serviceregistry.Dial* DEPRECATED: switch to kitex <svc>service.NewClient")

// HardenedServerOptions 老 gRPC server keepalive 配置 — Kitex 自带 keepalive,
// 这个函数现在返空切片, 保留只为兼容 kms-manage / accounting-system / risk-manage 等
// 还有引用. 真切 Kitex 后这些调用都不再起作用 (kitex 不接受 grpc.ServerOption).
func HardenedServerOptions() []grpc.ServerOption {
	return nil
}

// MTLSDialOptions 老 gRPC mTLS DialOption — mTLS 已不需要 (内部 mesh).
// 返空切片让老 caller 编译过.
func MTLSDialOptions() []grpc.DialOption {
	return nil
}

// Dial DEPRECATED.
func Dial(service string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return nil, ErrDeprecated
}

// DialWithFallback DEPRECATED.
func DialWithFallback(endpoints []string, service, fallbackAddr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return nil, ErrDeprecated
}

// DialDirect DEPRECATED.
func DialDirect(endpoint string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return nil, ErrDeprecated
}

// DialFromEndpoints DEPRECATED.
func DialFromEndpoints(endpoints []string, service string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return nil, ErrDeprecated
}

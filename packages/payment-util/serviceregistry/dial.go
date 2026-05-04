package serviceregistry

import (
	"fmt"

	"google.golang.org/grpc"
)

// roundRobinServiceConfig 让 grpc-go 给 resolver 返回的每个端点都建一条子连接，
// RPC 在子连接间轮询；resolver 推送变更（上线/下线）后自动 rebalance。
const roundRobinServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// Dial creates a gRPC ClientConn for the named service via the etcd resolver
// (RegisterResolver must have run first). It always sets round_robin LB so
// traffic spreads across all live replicas.
//
// 用户传入的 opts 可以覆盖默认 service config（如想自定义 retry / timeout policy）。
// Caller-provided WithDefaultServiceConfig wins because 它出现在 fixed 之后。
func Dial(service string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if service == "" {
		return nil, fmt.Errorf("serviceregistry: empty service name")
	}
	target := fmt.Sprintf("%s:///%s", resolverScheme, service)

	fixed := []grpc.DialOption{
		grpc.WithDefaultServiceConfig(roundRobinServiceConfig),
	}
	fixed = append(fixed, opts...)
	return grpc.NewClient(target, fixed...)
}

// DialWithFallback 是面向"既要支持 etcd resolver 又要支持单仓 dev 直连"的统一入口。
//
//	endpoints 非空 → 走 DialFromEndpoints(etcd resolver + round_robin)
//	endpoints 为空 → 退回 grpc.NewClient(fallbackAddr) + 默认 round_robin
//
// 所有 daemon 调用方应该用本 helper 代替裸 grpc.NewClient(addr)，这样 docker
// 联栈模式下（容器删了 container_name 改用 --scale）能通过 etcd 而不是 DNS
// 跨项目寻址；本地 dev / 单仓 docker run 模式（无 etcd）退回直连仍然 work。
//
// fallbackAddr 在 endpoints 非空时被忽略；调用方仍要传以便降级 + 日志可读。
func DialWithFallback(endpoints []string, service, fallbackAddr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if len(endpoints) > 0 {
		if service == "" {
			return nil, fmt.Errorf("serviceregistry: empty service name")
		}
		return DialFromEndpoints(endpoints, service, opts...)
	}
	if fallbackAddr == "" {
		return nil, fmt.Errorf("serviceregistry: empty endpoints and empty fallbackAddr for service %q", service)
	}
	// 直连模式也加 round_robin：DNS 解析返回多 A 记录时（k8s headless svc /
	// docker network 多副本同 alias）才会均摊。
	fixed := []grpc.DialOption{
		grpc.WithDefaultServiceConfig(roundRobinServiceConfig),
	}
	fixed = append(fixed, opts...)
	return grpc.NewClient(fallbackAddr, fixed...)
}

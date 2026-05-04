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

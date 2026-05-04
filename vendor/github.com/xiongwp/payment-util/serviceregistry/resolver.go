package serviceregistry

import (
	"fmt"
	"sync"

	clientv3 "go.etcd.io/etcd/client/v3"
	etcdresolver "go.etcd.io/etcd/client/v3/naming/resolver"
	"google.golang.org/grpc/resolver"
)

// resolverScheme is the gRPC target scheme this package registers ("etcd").
// Targets look like "etcd:///<service>" — three slashes since etcd watches
// the registered service prefix without an authority component.
const resolverScheme = "etcd"

var (
	resolverOnce sync.Once
	resolverErr  error
)

// RegisterResolver registers an etcd-based gRPC resolver under the
// "etcd" scheme. Safe to call multiple times — only the first call takes
// effect (gRPC's resolver registry is process-global). Subsequent calls
// with a different etcd client are no-ops.
//
// After this call, grpc.NewClient("etcd:///<service>") works for any
// service whose Registrar has published to the same etcd cluster.
func RegisterResolver(client *clientv3.Client) error {
	if client == nil {
		return fmt.Errorf("serviceregistry: nil etcd client")
	}
	resolverOnce.Do(func() {
		b, err := etcdresolver.NewBuilder(client)
		if err != nil {
			resolverErr = fmt.Errorf("serviceregistry: build etcd resolver: %w", err)
			return
		}
		// etcd builder reports its own scheme ("etcd")；resolver.Register
		// 只走第一个匹配 scheme 的 builder。
		resolver.Register(b)
	})
	return resolverErr
}

// ResolverScheme returns "etcd" — the scheme prefix to use in target strings.
func ResolverScheme() string { return resolverScheme }

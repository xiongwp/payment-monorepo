// etcd.go — etcd client 构造 helper, 给 Server (EtcdRegistry) 和
// Client (EtcdResolver) 共用. 解决重复 dial / config 漂移问题.
//
// 用法 (server / client 都是这条入口):
//
//	cli, err := kitexutil.NewEtcdClient(registryEndpoints, 5*time.Second)
//	if err != nil { ... }
//	defer cli.Close()
package kitexutil

import (
	"fmt"
	"os"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// NewEtcdClient 构造 etcd v3 client.
//
//   - endpoints: ["etcd:2379"] / ["etcd1:2379","etcd2:2379","etcd3:2379"];
//     dev 联调单节点; prod 必须 3+ 节点 (etcd Raft quorum).
//   - dialTimeout: 拨号阶段超时; 之后的 RPC 用各自 ctx.
//
// 不立刻 health-check (etcd v3 client 是 lazy 的), 真正不可达会在第一次 Get/Grant
// 时返回. 让 caller 自己决定 fail-fast 还是 retry.
func NewEtcdClient(endpoints []string, dialTimeout time.Duration) (*clientv3.Client, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("kitexutil: etcd endpoints empty")
	}
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("kitexutil: dial etcd %v: %w", endpoints, err)
	}
	return cli, nil
}

// RegistryEndpointsFromEnv 从 env REGISTRY_ENDPOINTS 读 etcd endpoints.
// 支持逗号分隔: "etcd1:2379,etcd2:2379,etcd3:2379".
// 空字符串 / 未设置 → 返空 slice; 上层决定 fallback 行为.
func RegistryEndpointsFromEnv() []string {
	v := strings.TrimSpace(os.Getenv("REGISTRY_ENDPOINTS"))
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

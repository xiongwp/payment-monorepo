// resolver.go — Kitex client 服务发现 (etcd 后端).
//
// 跟 payment-util/serviceregistry.Registrar (旧 gRPC 注册器) 用同一份 etcd 数据:
//
//	etcd key 格式: <service>/<addr>    (例: accounting-service/10.0.0.5:50051)
//	etcd value:    "addr=10.0.0.5:50051" 或 etcd endpoints.Endpoint JSON
//
// 这样 Kitex 时代和 gRPC 时代的注册数据互通, 切换可以渐进 (一边的 server 先切, 另一边
// 的 client 仍能发现; 反之亦然).

package kitexutil

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdResolver 把 etcd 里的服务实例列表暴露给 Kitex client. 数据格式跟
// serviceregistry.Registrar 完全一致 (`<svcname>/<addr>` 平铺 key), 老 gRPC 注册的
// 数据 Kitex client 直接复用.
type EtcdResolver struct {
	cli *clientv3.Client

	mu    sync.RWMutex
	cache map[string][]discovery.Instance // service → instances
}

// NewEtcdResolver 用现有 etcd client 构造 resolver.
//
// 第二个参数 prefix 保留向后兼容签名, 当前实现不使用 — etcd 数据格式跟 etcd 官方
// endpoints.Manager 对齐 (flat <svcname>/<addr> key, 无业务前缀).
func NewEtcdResolver(cli *clientv3.Client, prefix string) *EtcdResolver {
	_ = prefix // 保留参数兼容 caller, 当前不使用
	return &EtcdResolver{
		cli:   cli,
		cache: map[string][]discovery.Instance{},
	}
}

// Target Kitex Resolver 接口必须实现 — 返 service 名 (Kitex 用它作 cache key).
func (r *EtcdResolver) Target(_ context.Context, target rpcinfo.EndpointInfo) string {
	return target.ServiceName()
}

// Resolve 拉某 service 的所有实例.
//
// 行为:
//   - etcd 不可达 → 返 error, Kitex 走 retry 路径
//   - 空列表 → 返 ErrNoEndpoints, Kitex 短路, caller log 后报错
//   - etcd key 格式: <service>/<addr>  (例: "accounting-service/10.0.0.5:50051")
//   - value 兼容 etcd 官方 endpoints.Endpoint JSON 和 "addr=host:port,weight=10"
//     纯文本两种格式; 任一可解出 addr 就行.
//
// 重要: 这里的 "<service>" 名字必须跟 server 端 EtcdRegistrar 注册时用的同一份.
// docker DNS 名 vs 包名 不一致的, 以 EtcdRegistrar 写进 etcd 那个为准.
func (r *EtcdResolver) Resolve(ctx context.Context, desc string) (discovery.Result, error) {
	if r.cli == nil {
		return discovery.Result{}, errors.New("kitexutil: etcd client nil")
	}
	prefix := strings.TrimRight(desc, "/") + "/"
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := r.cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return discovery.Result{}, fmt.Errorf("etcd Get %s: %w", prefix, err)
	}
	if len(resp.Kvs) == 0 {
		// REG-REDESIGN: 错误信息带 svc 名 + endpoints, 让 ops 一眼看见是哪个服务没注册.
		return discovery.Result{CacheKey: desc, Cacheable: true},
			fmt.Errorf("%w: svc=%q (检查该服务有没有起 + REGISTRY_ENDPOINTS env)", ErrNoEndpoints, desc)
	}
	out := make([]discovery.Instance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		addr := strings.TrimPrefix(string(kv.Key), prefix)
		if addr == "" {
			continue
		}
		tags := parseTags(string(kv.Value))
		// 注册器写入的 value 是 etcd endpoints.Endpoint JSON, 形如
		// {"Addr":"10.0.0.5:50051","Metadata":null}. 用 Addr 字段覆盖 key
		// 里取的 addr (二者应该一致, 防 key/value 漂移以 value 为准).
		if a, ok := tags["Addr"]; ok && a != "" {
			addr = strings.Trim(a, "\"")
		}
		out = append(out, discovery.NewInstance("tcp", addr, 10, tags))
	}
	if len(out) == 0 {
		return discovery.Result{CacheKey: desc, Cacheable: true},
			fmt.Errorf("%w: svc=%q (etcd 有 key 但 addr 解析全空)", ErrNoEndpoints, desc)
	}
	r.mu.Lock()
	r.cache[desc] = out
	r.mu.Unlock()
	return discovery.Result{
		CacheKey:  desc,
		Cacheable: true,
		Instances: out,
	}, nil
}

// Diff Kitex Resolver 接口要求 — 计算两次 Resolve 之间的变化 (新增/删除).
// 简化实现: 返当前 next 作为最新值, 不算 delta (Kitex 内部用全量替换也 OK).
func (r *EtcdResolver) Diff(_ string, _, next discovery.Result) (discovery.Change, bool) {
	return discovery.Change{Result: next}, true
}

// Name Kitex Resolver 接口实现; log + metric 用.
func (r *EtcdResolver) Name() string { return "kitexutil-etcd" }

// CachedInstances 返回最后一次成功 Resolve 的快照 (服务发现 stale fallback 用).
func (r *EtcdResolver) CachedInstances(service string) []discovery.Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache[service]
}

// ErrNoEndpoints — etcd 里没找到任何实例.
var ErrNoEndpoints = errors.New("kitexutil: no etcd endpoints for service")

// 编译期接口断言: EtcdResolver 必须实现 discovery.Resolver, 否则 Kitex client
// 装载时会爆 "cannot use ... as discovery.Resolver value" — 提前在 build 期暴露.
var _ discovery.Resolver = (*EtcdResolver)(nil)

func parseTags(value string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(value, ",") {
		kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
		}
	}
	return out
}

// HostPortsFromInstances 工具函数: 把 discovery.Instance 列表转 host:port 数组,
// 给不支持 resolver 的简化场景用 (e.g. test bench / dev script).
func HostPortsFromInstances(insts []discovery.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		if a := i.Address(); a != nil {
			host, port, err := net.SplitHostPort(a.String())
			if err == nil {
				out = append(out, net.JoinHostPort(host, port))
			} else {
				out = append(out, a.String())
			}
		}
	}
	return out
}

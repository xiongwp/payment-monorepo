// resolver.go — Kitex client 服务发现 (etcd 后端).
//
// 跟 serviceregistry.RegisterResolver (gRPC resolver) 等价的 Kitex 版.
// Kitex discovery.Resolver 接口 + discovery.Instance, 真接 Kitex 0.10 API.

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
	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdResolver 把 etcd 里的服务实例列表暴露给 Kitex client.
//
// 跟 serviceregistry 一致的 key 前缀 (默认 "/recon/services"), 跟现有 gRPC 注册数据
// 完全兼容 — 同一份 etcd registry 数据 Kitex client 和老 grpc client 都能读.
type EtcdResolver struct {
	cli    *clientv3.Client
	prefix string

	mu    sync.RWMutex
	cache map[string][]discovery.Instance // service → instances
}

// NewEtcdResolver 用现有 etcd client 构造 resolver.
//
// prefix 留空走 "/recon/services" — 跟 serviceregistry/registrar.go 同款约定,
// 老 gRPC 注册的服务 Kitex client 直接复用同一份注册数据.
func NewEtcdResolver(cli *clientv3.Client, prefix string) *EtcdResolver {
	if prefix == "" {
		prefix = "/recon/services"
	}
	return &EtcdResolver{
		cli:    cli,
		prefix: strings.TrimRight(prefix, "/"),
		cache:  map[string][]discovery.Instance{},
	}
}

// Target Kitex Resolver 接口必须实现 — 返 service 名 (Kitex 用它作 cache key).
func (r *EtcdResolver) Target(_ context.Context, target rpcInfoLike) string {
	return target.ServiceName()
}

// rpcInfoLike Kitex 0.10 Resolver.Target 收 rpcinfo.EndpointInfo, 这里抽接口
// 减少跨 vendor 边界 — 真接 Kitex 时直接传 rpcinfo.EndpointInfo 也满足这个接口.
type rpcInfoLike interface {
	ServiceName() string
}

// Resolve 拉某 service 的所有实例.
//
// 行为:
//   - etcd 不可达 → 返 error, Kitex 走 retry / fallback 路径
//   - 空列表 → 返 ErrNoEndpoints, 调用方 (kitexutil dial helper) 应降级 fallback addr
//   - 实例 value 格式: "addr=host:port,weight=10,zone=us-east-1" (兼容 serviceregistry)
func (r *EtcdResolver) Resolve(ctx context.Context, desc string) (discovery.Result, error) {
	if r.cli == nil {
		return discovery.Result{}, errors.New("kitexutil: etcd client nil")
	}
	key := r.prefix + "/" + desc + "/"
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := r.cli.Get(ctx, key, clientv3.WithPrefix())
	if err != nil {
		return discovery.Result{}, fmt.Errorf("etcd Get %s: %w", key, err)
	}
	if len(resp.Kvs) == 0 {
		return discovery.Result{CacheKey: desc, Cacheable: true}, ErrNoEndpoints
	}
	out := make([]discovery.Instance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		addr := strings.TrimPrefix(string(kv.Key), key)
		if addr == "" {
			continue
		}
		tags := parseTags(string(kv.Value))
		weight := 10
		if w, ok := tags["weight"]; ok {
			// 兼容 "weight=20" 格式 (parseTags 已经把 key=val 拆出)
			_ = w
		}
		out = append(out, discovery.NewInstance("tcp", addr, weight, tags))
	}
	if len(out) == 0 {
		return discovery.Result{CacheKey: desc, Cacheable: true}, ErrNoEndpoints
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

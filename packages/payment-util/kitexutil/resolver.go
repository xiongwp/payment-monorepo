// resolver.go — Kitex 客户端服务发现 (etcd 后端).
//
// 跟 serviceregistry.RegisterResolver (gRPC resolver) 等价的 Kitex 版.
// Kitex 用 discovery.Resolver 接口, 跟 grpc 的 resolver.Builder 形态不同, 这里桥接 etcd.

package kitexutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Instance 跟 Kitex discovery.Instance 接口形态对齐 (避免直接依赖 Kitex package,
// 让 payment-util 不强绑 Kitex 版本; 真实代码生成期 cast 一下即可).
type Instance interface {
	Address() string // host:port
	Weight() int     // 跟 RR LB 权重对齐, 默认 10
	Tag(key string) (value string, exist bool)
}

// instanceImpl 内部实例实现 — 启动期从 etcd value 反序列化得到.
type instanceImpl struct {
	addr   string
	weight int
	tags   map[string]string
}

func (i *instanceImpl) Address() string { return i.addr }
func (i *instanceImpl) Weight() int     { return i.weight }
func (i *instanceImpl) Tag(k string) (string, bool) {
	v, ok := i.tags[k]
	return v, ok
}

// EtcdResolver 把 etcd 里的服务实例列表暴露给 Kitex client.
//
// 跟 serviceregistry 一致的 key 前缀: "/recon/services/<service>/<addr>".
// 一次 Resolve 返回当前快照, Kitex 自己周期性 Resolve 拿最新.
type EtcdResolver struct {
	cli    *clientv3.Client
	prefix string

	mu    sync.RWMutex
	cache map[string][]Instance // service → instances
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
		cache:  map[string][]Instance{},
	}
}

// Target Kitex Resolver 接口必须实现, 返 service 名作为 cache key.
func (r *EtcdResolver) Target(ctx context.Context, service string) string {
	return service
}

// Resolve 拉某 service 的所有实例.
//
// 行为:
//   - etcd 不可达 → 返 error, Kitex 走 retry / fallback 路径
//   - 空列表 → 返 ErrNoEndpoints, 上游 (kitexutil dial helper) 应降级 fallback addr
//   - 实例 value 格式: "addr=host:port,weight=10,zone=us-east-1" (兼容 serviceregistry 写入)
func (r *EtcdResolver) Resolve(ctx context.Context, service string) ([]Instance, error) {
	if r.cli == nil {
		return nil, errors.New("kitexutil: etcd client nil (set RECON_ETCD_ENDPOINTS)")
	}
	key := r.prefix + "/" + service + "/"
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	resp, err := r.cli.Get(ctx, key, clientv3.WithPrefix())
	if err != nil {
		return nil, fmt.Errorf("etcd Get %s: %w", key, err)
	}
	if len(resp.Kvs) == 0 {
		return nil, ErrNoEndpoints
	}
	out := make([]Instance, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		// addr 直接取 key 末段; tags 从 value 解 — 兼容 serviceregistry 老写入.
		addr := strings.TrimPrefix(string(kv.Key), key)
		if addr == "" {
			continue
		}
		out = append(out, &instanceImpl{
			addr:   addr,
			weight: 10,
			tags:   parseTags(string(kv.Value)),
		})
	}
	if len(out) == 0 {
		return nil, ErrNoEndpoints
	}
	// 缓存一份 — 下次 Resolve 失败时可降级返旧快照.
	r.mu.Lock()
	r.cache[service] = out
	r.mu.Unlock()
	return out, nil
}

// CachedInstances 返回最后一次成功 Resolve 的快照 (服务发现 stale fallback 用).
func (r *EtcdResolver) CachedInstances(service string) []Instance {
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

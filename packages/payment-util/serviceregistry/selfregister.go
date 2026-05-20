// Package serviceregistry — leader election + shared etcd client helpers.
//
// REG-REDESIGN: service self-registration 已迁移到 payment-util/kitexutil.
// 本文件之前定义的 RegisterSelf / SelfRegistration / Registrar 已删, 因为它们
// 跟 kitexutil.EtcdRegistry 重复写同一份 etcd 数据 (不同 lease, 不同 addr 格式),
// 导致 caller 拿到 stale endpoint 拨号失败.
//
// 剩下的内容仅给 leader election + client-side probing 用:
//   - EnsureEtcdClient / SharedEtcdClient: leader 选举共享 etcd client
//   - HasRegisteredInstances: BFF 启动期探测 etcd 有没有目标服务的端点
//     (BFF 决定是否走 etcd resolver 或者降级直连 fallbackAddr)
package serviceregistry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// ─── client-side convenience (leader election 复用) ──────────────────────────

var (
	sharedClientMu  sync.Mutex
	sharedClient    *clientv3.Client
	sharedClientErr error
)

func initSharedResolver(endpoints []string) error {
	sharedClientMu.Lock()
	defer sharedClientMu.Unlock()
	if sharedClient != nil {
		return nil
	}
	if sharedClientErr != nil {
		return sharedClientErr
	}
	cli, err := NewEtcdClient(endpoints)
	if err != nil {
		sharedClientErr = err
		return err
	}
	sharedClient = cli
	return nil
}

// EnsureEtcdClient 启动期初始化共享 etcd client (跟 leader election 复用).
func EnsureEtcdClient(endpoints []string) error {
	return initSharedResolver(endpoints)
}

// SharedEtcdClient returns the etcd client created by EnsureEtcdClient
// (or nil if none yet). Useful for sharing a client with NewElection.
func SharedEtcdClient() *clientv3.Client {
	sharedClientMu.Lock()
	defer sharedClientMu.Unlock()
	return sharedClient
}

// ErrNoEndpoints is returned when an etcd helper is called with no endpoints.
var ErrNoEndpoints = errNoEndpoints{}

type errNoEndpoints struct{}

func (errNoEndpoints) Error() string { return "serviceregistry: no etcd endpoints configured" }

// HasRegisteredInstances 探测 etcd 里是否有 service 的注册条目.
// BFF 启动期 etcd 配了但 service 还没注册时, 调用方可以跳过 etcd resolver 直接
// 走 fallbackAddr 直连, 避免 round_robin balancer 因 0 个 SubConn 报
// "no children to pick from".
//
// 协议契约 (跟 kitexutil.EtcdRegistry.Register 写入格式对齐):
//
//	key 形态: <service>/<addr>   (e.g. "kms-manage/10.0.1.7:9290")
//	所以 prefix Get "<service>/" 能拿到所有 live 副本数.
func HasRegisteredInstances(endpoints []string, service string, timeout time.Duration) (bool, error) {
	if len(endpoints) == 0 || service == "" {
		return false, nil
	}
	cli, err := NewEtcdClient(endpoints)
	if err != nil {
		return false, fmt.Errorf("serviceregistry: probe new client: %w", err)
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	prefix := strings.TrimRight(service, "/") + "/"
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return false, fmt.Errorf("serviceregistry: probe get %q: %w", prefix, err)
	}
	return resp.Count > 0, nil
}

// registrar.go — Kitex server 端 etcd 自注册.
//
// 跟 payment-util/serviceregistry.Registrar (旧 gRPC 时代的注册器) 用同一份 etcd
// 数据 (etcd 官方 endpoints.Manager, key 格式 "<svcname>/<addr>"), 让 Kitex client
// (EtcdResolver) 和老 gRPC client (grpc/resolver.Builder) 都能读到同一组实例.
//
// 用法:
//
//	registry, err := kitexutil.NewEtcdRegistry(etcdCli, "accounting-service", "10.0.0.5:50051", 30*time.Second)
//	srv := kitexserver.NewServer(
//	    kitexserver.WithServiceAddr(addr),
//	    kitexserver.WithRegistry(registry),
//	    kitexserver.WithRegistryInfo(&registry108.Info{ServiceName: "accounting-service", Addr: utils.NewNetAddr("tcp", "10.0.0.5:50051")}),
//	)
//
// 关停时 srv.Stop() → Kitex 自动调 registry.Deregister(...) → revoke lease →
// etcd 立刻删 endpoint, 上游 client 5s 内感知到摘除.
//
// 进程崩溃 (没机会 Deregister) 时, TTL 过期 → etcd lease 自动失效 → endpoint
// 也被删. TTL 30s 默认, 配合 server-side keepalive 大概 10s 续一次, 进程死后
// 最多 30s 才被剔除.
package kitexutil

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/kitex/pkg/registry"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"
)

// EtcdRegistry Kitex server.Registry 实现; 跟 serviceregistry.Registrar 用
// 同一份数据格式 (etcd 官方 endpoints.Endpoint JSON).
//
// REG-TTL: keepalive channel 关闭时自动重 grant + AddEndpoint, 防止 etcd 短暂
// 失联后 entry 永久消失. 老实现只 drain channel, 必须重启进程才能恢复.
type EtcdRegistry struct {
	cli     *clientv3.Client
	service string // 注册时用的 service 名 (= docker DNS 名, 不是包名)
	addr    string // 注册的 host:port (advertise 给上游用的, 必须可路由)
	ttl     time.Duration

	mu        sync.Mutex
	manager   endpoints.Manager
	leaseID   clientv3.LeaseID
	keepCanc  context.CancelFunc
	closeOnce sync.Once
	stopped   bool // Deregister 之后置 true, reconnect goroutine 看到就退出
}

// NewEtcdRegistry 构造 (不立刻注册; Register 时才写 etcd).
//
//   - service: 必填. 注册到 etcd 的服务名, 跟 caller EtcdResolver 解析时用的同名.
//   - addr:    必填. 注册广播的 host:port. dev 用 docker DNS 名 (如 "accounting-service:50051"),
//     prod 用 pod IP (如 "10.0.0.5:50051").
//   - ttl:     lease TTL, 默认 30s; 太短了 keepalive 来不及容易抖动, 太长崩溃后摘除慢.
func NewEtcdRegistry(cli *clientv3.Client, service, addr string, ttl time.Duration) (*EtcdRegistry, error) {
	if cli == nil {
		return nil, fmt.Errorf("kitexutil: nil etcd client")
	}
	if service == "" || addr == "" {
		return nil, fmt.Errorf("kitexutil: empty service or addr")
	}
	if ttl < time.Second {
		ttl = 30 * time.Second
	}
	mgr, err := endpoints.NewManager(cli, service)
	if err != nil {
		return nil, fmt.Errorf("kitexutil: endpoints manager: %w", err)
	}
	return &EtcdRegistry{
		cli:     cli,
		service: service,
		addr:    addr,
		ttl:     ttl,
		manager: mgr,
	}, nil
}

// Register Kitex server.Registry 接口. 启动期由 kitexserver 自动调.
//
// info.ServiceName 一般等于 NewEtcdRegistry 传入的 service; Kitex 默认行为是用
// server.WithRegistryInfo() 注入的 Info, 这里保留 info 优先级 (覆盖 ctor 默认).
func (r *EtcdRegistry) Register(info *registry.Info) error {
	if info == nil {
		return fmt.Errorf("kitexutil: nil registry info")
	}
	svc := info.ServiceName
	if svc == "" {
		svc = r.service
	}
	// addr 来源: info.Addr (server.WithServiceAddr 提供的) > ctor 时传的 r.addr
	addr := r.addr
	if info.Addr != nil {
		addr = canonicalAddr(info.Addr)
	}
	if addr == "" {
		return fmt.Errorf("kitexutil: empty advertise addr")
	}
	// 落到结构体里供 reconnect 用 (Kitex 第一次 Register 后值固定).
	r.mu.Lock()
	r.service = svc
	r.addr = addr
	r.mu.Unlock()

	return r.doRegisterOnce()
}

// doRegisterOnce 一次完整的 grant + AddEndpoint + keepalive 装载; 失败保留
// r.leaseID=0 让 caller 知道还没注册. 不更新 r.service/r.addr (已在 Register 设).
func (r *EtcdRegistry) doRegisterOnce() error {
	r.mu.Lock()
	svc, addr := r.service, r.addr
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := r.cli.Grant(ctx, int64(r.ttl.Seconds()))
	if err != nil {
		return fmt.Errorf("kitexutil: etcd grant lease: %w", err)
	}

	key := fmt.Sprintf("%s/%s", svc, addr)
	if err := r.manager.AddEndpoint(ctx, key,
		endpoints.Endpoint{Addr: addr},
		clientv3.WithLease(lease.ID)); err != nil {
		_, _ = r.cli.Revoke(ctx, lease.ID)
		return fmt.Errorf("kitexutil: add endpoint %q: %w", key, err)
	}

	keepCtx, cancelKA := context.WithCancel(context.Background())
	ch, err := r.cli.KeepAlive(keepCtx, lease.ID)
	if err != nil {
		cancelKA()
		_ = r.manager.DeleteEndpoint(ctx, key)
		_, _ = r.cli.Revoke(ctx, lease.ID)
		return fmt.Errorf("kitexutil: keepalive: %w", err)
	}

	r.mu.Lock()
	stopped := r.stopped
	if !stopped {
		r.leaseID = lease.ID
		r.keepCanc = cancelKA
	}
	r.mu.Unlock()

	if stopped {
		cancelKA()
		_, _ = r.cli.Revoke(context.Background(), lease.ID)
		return nil
	}

	go r.keepaliveLoop(ch)
	return nil
}

// keepaliveLoop drain 心跳; channel 关闭 (etcd 失联 / lease 过期) 后自动重 grant.
// Deregister 把 r.stopped 置 true 通知本 goroutine 永久退出.
func (r *EtcdRegistry) keepaliveLoop(ch <-chan *clientv3.LeaseKeepAliveResponse) {
	for range ch {
		// 心跳正常, 啥也不做
	}
	r.mu.Lock()
	stopped := r.stopped
	r.leaseID = 0
	if r.keepCanc != nil {
		r.keepCanc()
		r.keepCanc = nil
	}
	r.mu.Unlock()
	if stopped {
		return
	}
	backoff := time.Second
	for {
		r.mu.Lock()
		if r.stopped {
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		if err := r.doRegisterOnce(); err == nil {
			return
		}
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// Deregister Kitex server.Registry 接口. 关停时调, 立刻 revoke lease 删 endpoint.
// 进程崩溃情况下 lease TTL 过期后由 etcd 自动清理.
//
// REG-TTL: 同时通过 r.stopped 通知 keepaliveLoop 永久退出 (防它在 graceful
// shutdown 期间又重新 grant 一个新的 lease).
func (r *EtcdRegistry) Deregister(info *registry.Info) error {
	var firstErr error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.stopped = true
		leaseID := r.leaseID
		cancel := r.keepCanc
		r.leaseID = 0
		r.keepCanc = nil
		r.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if leaseID != 0 {
			ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
			defer c()
			if _, err := r.cli.Revoke(ctx, leaseID); err != nil {
				firstErr = fmt.Errorf("kitexutil: revoke: %w", err)
			}
		}
	})
	_ = info
	return firstErr
}

// canonicalAddr 把 net.Addr 转 "host:port" 字符串.
// Kitex 默认传 *net.TCPAddr, 直接 .String() 也行; 这里防御性 split + 拼.
func canonicalAddr(a net.Addr) string {
	s := a.String()
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return s
	}
	// 0.0.0.0 / :: 表示监听全部接口, etcd 注册需要可路由 IP, 这种情况调用方
	// 应该传具体 advertise 地址 (env ADVERTISE_HOST 之类); 这里只做最低限度
	// fallback: 改用本机 hostname (docker 网络下能解析).
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = guessHost()
	}
	return net.JoinHostPort(host, port)
}

func guessHost() string {
	// dev 默认: docker 容器内 hostname 跟 service-name 一致, 返 hostname.
	// prod: 调用方应该用 POD_IP env 显式覆盖, 不走这条 fallback.
	if h, err := osHostname(); err == nil && h != "" {
		return strings.TrimSpace(h)
	}
	return "127.0.0.1"
}

// osHostname — 抽出来方便测试 mock.
var osHostname = func() (string, error) {
	return os.Hostname()
}

// 编译期接口断言: EtcdRegistry 必须实现 registry.Registry, 否则
// kitexserver.WithRegistry(...) 装载时会爆 "cannot use ... as registry.Registry
// value" — 提前在 build 期暴露, 别等到运行时.
var _ registry.Registry = (*EtcdRegistry)(nil)

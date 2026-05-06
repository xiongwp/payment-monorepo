package serviceregistry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// SelfRegistration bundles the etcd client + Registrar created by RegisterSelf,
// so callers can defer a single Close on graceful shutdown. Created etcd client
// is owned by the SelfRegistration and closed on Close.
type SelfRegistration struct {
	Client *clientv3.Client
	Reg    *Registrar
}

// Close revokes the etcd lease (immediate endpoint removal) and shuts the
// embedded etcd client. Safe to call multiple times; safe to call on nil.
func (s *SelfRegistration) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	if s.Reg != nil {
		if err := s.Reg.Close(); err != nil {
			firstErr = err
		}
	}
	if s.Client != nil {
		if err := s.Client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RegisterSelf is a one-shot boot-time helper for service self-registration:
// creates an etcd client → builds a Registrar → grants a lease + writes the
// endpoint + spins up keepalive. Returns (nil, nil) when endpoints is empty
// (signal that service registry is disabled / dev mode); the caller can
// continue without etcd in that case.
//
// advertiseAddr is the "host:port" remote callers will dial through the
// resolver. Default to os.Hostname() + service port; override with
// REGISTRY_ADVERTISE_ADDR env for K8s POD_IP.
func RegisterSelf(ctx context.Context, endpoints []string, service, advertiseAddr string, ttl time.Duration) (*SelfRegistration, error) {
	if len(endpoints) == 0 {
		return nil, nil
	}
	cli, err := NewEtcdClient(endpoints)
	if err != nil {
		return nil, err
	}
	reg, err := NewRegistrar(cli, service, advertiseAddr)
	if err != nil {
		_ = cli.Close()
		return nil, err
	}
	if err := reg.Register(ctx, ttl); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return &SelfRegistration{Client: cli, Reg: reg}, nil
}

// ─── client-side convenience ─────────────────────────────────────────────

var (
	sharedClientMu  sync.Mutex
	sharedClient    *clientv3.Client
	sharedClientErr error
)

// DialFromEndpoints creates a gRPC ClientConn for `service` using etcd-based
// service discovery. Idempotent: the underlying etcd client + resolver are
// initialized once per process (first call wins).
//
// On endpoints == nil/empty the call returns ErrNoEndpoints; caller should
// fall back to direct grpc.NewClient when running without etcd.
func DialFromEndpoints(endpoints []string, service string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	if len(endpoints) == 0 {
		return nil, ErrNoEndpoints
	}
	if err := initSharedResolver(endpoints); err != nil {
		return nil, err
	}
	return Dial(service, opts...)
}

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
	if err := RegisterResolver(cli); err != nil {
		_ = cli.Close()
		sharedClientErr = err
		return err
	}
	sharedClient = cli
	return nil
}

// SharedEtcdClient returns the etcd client created by the first DialFromEndpoints
// call (or nil if none yet). Useful for sharing a client with NewElection.
func SharedEtcdClient() *clientv3.Client {
	sharedClientMu.Lock()
	defer sharedClientMu.Unlock()
	return sharedClient
}

// ErrNoEndpoints is returned when DialFromEndpoints is called with no etcd
// endpoints; callers can branch on this to fall back to direct dial.
var ErrNoEndpoints = errNoEndpoints{}

type errNoEndpoints struct{}

func (errNoEndpoints) Error() string { return "serviceregistry: no etcd endpoints configured" }

// HasRegisteredInstances 探测 etcd 里是否有 service 的注册条目。
// 用于客户端启动期：etcd 配了但 service 还没注册时，调用方可以跳过 etcd
// resolver 直接走 fallbackAddr 直连，避免 round_robin balancer 因为 0 个
// SubConn 报 "no children to pick from"。
//
// 协议契约（跟 Registrar.Register 写入格式对齐）：
//
//	key 形态：<service>/<addr>      (e.g. "kms-manage/10.0.1.7:9290")
//	所以 prefix Get "<service>/" 能拿到所有 live 副本数。
//
// 参数：
//   - endpoints：etcd cluster endpoints；空切片直接返 false（端到端短路 dev 模式）
//   - service  ：注册时用的 service name
//   - timeout  ：probe 超时；建议 2s，不要把 BFF 启动期拖到分钟级
//
// 返回：
//   - true  : etcd 上至少 1 个 <service>/* key
//   - false : 0 个 key 或 endpoints 空 / etcd 不可达（caller 应降级直连）
//   - err   : etcd 自身报错（区别于 0 entries 情况），调用方可只记日志不阻塞
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

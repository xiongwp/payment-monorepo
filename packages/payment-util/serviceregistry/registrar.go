package serviceregistry

import (
	"context"
	"fmt"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"
)

// Registrar binds (service, addr) to a TTL lease in etcd and keeps the lease
// alive in the background. On Close the lease is revoked, removing the
// endpoint immediately; on crash the lease expires within TTL.
//
// REG-TTL: keepalive channel 关闭时 (etcd 短暂失联 / 网络抖动) 自动重新
// Grant lease + AddEndpoint, 防止 entry 永久消失. 老实现只 drain channel,
// 一次 blip 就要求重启进程才能恢复.
type Registrar struct {
	client  *clientv3.Client
	manager endpoints.Manager
	service string
	addr    string
	ttl     time.Duration // 记下来给 reconnect 用

	mu        sync.Mutex
	leaseID   clientv3.LeaseID
	cancelKA  context.CancelFunc
	closeOnce sync.Once
	stopped   bool // closeOnce 之后置 true, reconnect goroutine 看到就退出
}

// NewRegistrar prepares (but does not yet register) an endpoint.
// Call Register to actually publish to etcd.
func NewRegistrar(client *clientv3.Client, service, addr string) (*Registrar, error) {
	if client == nil {
		return nil, fmt.Errorf("serviceregistry: nil etcd client")
	}
	if service == "" || addr == "" {
		return nil, fmt.Errorf("serviceregistry: empty service or addr")
	}
	em, err := endpoints.NewManager(client, service)
	if err != nil {
		return nil, fmt.Errorf("serviceregistry: endpoints manager: %w", err)
	}
	return &Registrar{
		client:  client,
		manager: em,
		service: service,
		addr:    addr,
	}, nil
}

// Register grants a lease, writes the endpoint, and starts a background
// keepalive goroutine. Idempotent for the lifetime of the Registrar — calling
// twice returns an error.
//
// REG-TTL: keepalive channel 关闭后 (etcd 失联 / lease expired) 自动重 grant +
// AddEndpoint, 防止 entry 永久消失. 重试 backoff 1s-30s 指数退避, Close 后退出.
func (r *Registrar) Register(ctx context.Context, ttl time.Duration) error {
	if ttl < time.Second {
		return fmt.Errorf("serviceregistry: ttl must be >= 1s, got %v", ttl)
	}
	r.mu.Lock()
	if r.leaseID != 0 {
		r.mu.Unlock()
		return fmt.Errorf("serviceregistry: already registered (lease %d)", r.leaseID)
	}
	r.ttl = ttl
	r.mu.Unlock()

	if err := r.doRegisterOnce(ctx); err != nil {
		return err
	}
	return nil
}

// doRegisterOnce 一次完整的 grant + AddEndpoint + keepalive 装载.
// 失败时不修改 r.leaseID, caller 决定是否重试.
func (r *Registrar) doRegisterOnce(ctx context.Context) error {
	lease, err := r.client.Grant(ctx, int64(r.ttl.Seconds()))
	if err != nil {
		return fmt.Errorf("serviceregistry: grant lease: %w", err)
	}

	key := fmt.Sprintf("%s/%s", r.service, r.addr)
	if err := r.manager.AddEndpoint(ctx, key,
		endpoints.Endpoint{Addr: r.addr},
		clientv3.WithLease(lease.ID)); err != nil {
		_, _ = r.client.Revoke(ctx, lease.ID)
		return fmt.Errorf("serviceregistry: add endpoint %q: %w", key, err)
	}

	keepCtx, cancel := context.WithCancel(context.Background())
	ch, err := r.client.KeepAlive(keepCtx, lease.ID)
	if err != nil {
		cancel()
		_ = r.manager.DeleteEndpoint(ctx, key)
		_, _ = r.client.Revoke(ctx, lease.ID)
		return fmt.Errorf("serviceregistry: keepalive: %w", err)
	}

	r.mu.Lock()
	r.leaseID = lease.ID
	r.cancelKA = cancel
	stopped := r.stopped
	r.mu.Unlock()

	if stopped {
		// 在 grant/keepalive 期间 Close 已被调用; 立即清理.
		cancel()
		_, _ = r.client.Revoke(context.Background(), lease.ID)
		return nil
	}

	go r.keepaliveLoop(ch)
	return nil
}

// keepaliveLoop drain keepalive responses; channel 关闭则自动重新注册.
// Close() 通过 r.stopped 标志告知本 goroutine 永久退出.
func (r *Registrar) keepaliveLoop(ch <-chan *clientv3.LeaseKeepAliveResponse) {
	for range ch {
		// 正常心跳, 啥也不用做
	}
	// channel 已关闭. 看是 Close 关的还是 etcd 失联.
	r.mu.Lock()
	stopped := r.stopped
	r.leaseID = 0 // 旧 lease 失效, 清理状态
	if r.cancelKA != nil {
		r.cancelKA()
		r.cancelKA = nil
	}
	r.mu.Unlock()
	if stopped {
		return
	}
	// 后台重试. backoff 1s → 2s → 4s → ... 最大 30s.
	backoff := time.Second
	for {
		r.mu.Lock()
		if r.stopped {
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := r.doRegisterOnce(ctx)
		cancel()
		if err == nil {
			return // 重注成功, doRegisterOnce 内部已起新 keepaliveLoop
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

// Close revokes the lease (immediate endpoint removal) and stops keepalive.
// Safe to call multiple times.
//
// REG-TTL: 同时把 r.stopped 置 true, 通知 keepaliveLoop 永久退出 (防 graceful
// shutdown 期间又自动重新 grant 一个新 lease, 让 prod 的 K8s preStop drain
// 行为符合预期).
func (r *Registrar) Close() error {
	var firstErr error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.stopped = true
		leaseID := r.leaseID
		cancel := r.cancelKA
		r.leaseID = 0
		r.cancelKA = nil
		r.mu.Unlock()

		if cancel != nil {
			cancel()
		}
		if leaseID != 0 {
			ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
			defer c()
			if _, err := r.client.Revoke(ctx, leaseID); err != nil {
				firstErr = fmt.Errorf("serviceregistry: revoke: %w", err)
			}
		}
	})
	return firstErr
}

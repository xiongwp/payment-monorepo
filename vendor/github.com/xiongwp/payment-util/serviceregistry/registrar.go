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
type Registrar struct {
	client  *clientv3.Client
	manager endpoints.Manager
	service string
	addr    string

	mu        sync.Mutex
	leaseID   clientv3.LeaseID
	cancelKA  context.CancelFunc
	closeOnce sync.Once
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
func (r *Registrar) Register(ctx context.Context, ttl time.Duration) error {
	if ttl < time.Second {
		return fmt.Errorf("serviceregistry: ttl must be >= 1s, got %v", ttl)
	}
	r.mu.Lock()
	if r.leaseID != 0 {
		r.mu.Unlock()
		return fmt.Errorf("serviceregistry: already registered (lease %d)", r.leaseID)
	}
	r.mu.Unlock()

	lease, err := r.client.Grant(ctx, int64(ttl.Seconds()))
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
	r.mu.Unlock()

	// 后台 drain keepalive responses；channel 关闭意味着 etcd 失联或 lease 失效。
	go func() {
		for range ch {
		}
	}()
	return nil
}

// Close revokes the lease (immediate endpoint removal) and stops keepalive.
// Safe to call multiple times.
func (r *Registrar) Close() error {
	var firstErr error
	r.closeOnce.Do(func() {
		r.mu.Lock()
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

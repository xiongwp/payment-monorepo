package serviceregistry

import (
	"context"
	"fmt"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

// Election wraps etcd's concurrency.Election: a process competes for
// ownership of a key; only one wins at a time. Used to gate cron / worker
// loops to a single replica across an N-pod deployment.
//
// 失去 leader 触发 Done()：可能因为 etcd 失联、session 过期，或 Resign。
// 调用方应该在收到 Done 后退出 cron 循环并重新 NewElection + Campaign。
type Election struct {
	client   *clientv3.Client
	key      string
	ttl      time.Duration
	mu       sync.Mutex
	session  *concurrency.Session
	election *concurrency.Election
}

// NewElection prepares an Election bound to the given etcd key (e.g.
// "/leader/order-core/expire-worker") with a session TTL.
//
// Note that the session is lazily created on first Campaign; this constructor
// validates inputs but does not yet contact etcd.
func NewElection(client *clientv3.Client, key string, ttl time.Duration) (*Election, error) {
	if client == nil {
		return nil, fmt.Errorf("serviceregistry: nil etcd client")
	}
	if key == "" {
		return nil, fmt.Errorf("serviceregistry: empty election key")
	}
	if ttl < time.Second {
		return nil, fmt.Errorf("serviceregistry: ttl must be >= 1s, got %v", ttl)
	}
	return &Election{client: client, key: key, ttl: ttl}, nil
}

// Campaign blocks until the caller is elected leader, the context is done,
// or the etcd session is invalidated. Returns nil on successful election.
//
// identity is the value associated with the leader key — useful for ops to
// see who's leading via etcdctl get / payment-util tooling. Hostname /
// pod name is a sensible choice.
func (e *Election) Campaign(ctx context.Context, identity string) error {
	if err := e.ensureSession(); err != nil {
		return err
	}
	e.mu.Lock()
	election := e.election
	e.mu.Unlock()
	return election.Campaign(ctx, identity)
}

// Resign steps down voluntarily; leadership goes to the next campaigner.
// Safe to call from leader only — calling on a non-leader returns nil.
func (e *Election) Resign(ctx context.Context) error {
	e.mu.Lock()
	election := e.election
	e.mu.Unlock()
	if election == nil {
		return nil
	}
	return election.Resign(ctx)
}

// Done returns a channel closed when the underlying session is invalidated.
// Caller must stop running leader-only logic when this fires.
func (e *Election) Done() <-chan struct{} {
	e.mu.Lock()
	sess := e.session
	e.mu.Unlock()
	if sess == nil {
		// No session yet → return a never-closed channel.
		ch := make(chan struct{})
		return ch
	}
	return sess.Done()
}

// Leader returns the current leader's identity, or "" if none / unknown.
// Useful for diagnostics; do NOT use as a "should I run?" gate (TOCTOU).
func (e *Election) Leader(ctx context.Context) (string, error) {
	if err := e.ensureSession(); err != nil {
		return "", err
	}
	e.mu.Lock()
	election := e.election
	e.mu.Unlock()
	resp, err := election.Leader(ctx)
	if err != nil {
		if err == concurrency.ErrElectionNoLeader {
			return "", nil
		}
		return "", err
	}
	if len(resp.Kvs) == 0 {
		return "", nil
	}
	return string(resp.Kvs[0].Value), nil
}

// Close terminates the session (releases leadership if held) and frees
// resources. Safe to call multiple times.
func (e *Election) Close() error {
	e.mu.Lock()
	sess := e.session
	e.session = nil
	e.election = nil
	e.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.Close()
}

func (e *Election) ensureSession() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.session != nil {
		return nil
	}
	sess, err := concurrency.NewSession(e.client, concurrency.WithTTL(int(e.ttl.Seconds())))
	if err != nil {
		return fmt.Errorf("serviceregistry: new session: %w", err)
	}
	e.session = sess
	e.election = concurrency.NewElection(sess, e.key)
	return nil
}

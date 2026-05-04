package handler

import (
	"sync"
	"time"
)

// idempotencyTTL is how long the BFF remembers an idempotency_key after the
// first successful response. 5 minutes covers the realistic double-click /
// retry window for an admin operator without growing the in-memory map.
//
// This cache is **best-effort**: it lives in a single BFF process so it does
// not survive restarts or LB resharding. End-to-end idempotency must still be
// enforced by the downstream gRPC service (payment-core / order-core) which
// MUST honour the idempotency_key header on Concede / Simulate / SubmitEvidence
// / CreateIntent. The BFF cache is purely a fast-path that prevents the
// trivial double-submit case from racing against a slow gRPC RTT.
const idempotencyTTL = 5 * time.Minute

// idempotencyEntry holds the cached response value plus its insertion time so
// the janitor can evict it after TTL.
type idempotencyEntry struct {
	value     interface{}
	expiresAt time.Time
}

// idempotencyStore is a TTL'd in-process map keyed by idempotency_key.
type idempotencyStore struct {
	mu    sync.RWMutex
	items map[string]idempotencyEntry
}

func newIdempotencyStore() *idempotencyStore {
	s := &idempotencyStore{items: make(map[string]idempotencyEntry, 64)}
	go s.gcLoop()
	return s
}

// Lookup returns the cached value for key if present and not expired.
func (s *idempotencyStore) Lookup(key string) (interface{}, bool) {
	if key == "" {
		return nil, false
	}
	s.mu.RLock()
	e, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().After(e.expiresAt) {
		s.mu.Lock()
		delete(s.items, key)
		s.mu.Unlock()
		return nil, false
	}
	return e.value, true
}

// Store remembers value under key for idempotencyTTL.
func (s *idempotencyStore) Store(key string, value interface{}) {
	if key == "" {
		return
	}
	s.mu.Lock()
	s.items[key] = idempotencyEntry{value: value, expiresAt: time.Now().Add(idempotencyTTL)}
	s.mu.Unlock()
}

// gcLoop runs forever, evicting stale entries every minute. Single-process
// daemon: leak is bounded by burst rate × TTL, which for an admin BFF is tiny.
func (s *idempotencyStore) gcLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for now := range t.C {
		s.mu.Lock()
		for k, e := range s.items {
			if now.After(e.expiresAt) {
				delete(s.items, k)
			}
		}
		s.mu.Unlock()
	}
}

// idempotencyCache is the package-global shared store used by all high-risk
// mutating handlers (concede / simulate / submit-evidence / create-intent /
// rotate-key). Cap is intentionally unbounded; janitor keeps it tame.
var idempotencyCache = newIdempotencyStore()

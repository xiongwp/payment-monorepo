// memory.go — 内存版 Layer,用于单测 + CLI 本地试跑 (不依赖 Redis).
//
// 行为与 Redis 版一致, TTL 不强制过期 (内存版认为测试不需要等 24h)。
package candidate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"reconcile-system/internal/store"
)

// MemoryLayer 内存 Layer.
type MemoryLayer struct {
	cfg     Config
	mu      sync.Mutex
	buckets map[string]map[string]*store.Event // bucketKey -> eventID -> event
	trigger []TriggerKey
	locks   map[string]bool
}

// NewMemoryLayer 构造.
func NewMemoryLayer(cfg Config) *MemoryLayer {
	if cfg.KeyPrefix == "" {
		cfg.KeyPrefix = "recon:cand"
	}
	if cfg.DefaultTriggerThreshold <= 0 {
		cfg.DefaultTriggerThreshold = 2
	}
	if cfg.DefaultTTL <= 0 {
		cfg.DefaultTTL = 24 * time.Hour
	}
	return &MemoryLayer{
		cfg:     cfg,
		buckets: map[string]map[string]*store.Event{},
		locks:   map[string]bool{},
	}
}

func (m *MemoryLayer) triggerN(bizKey string) int {
	if n, ok := m.cfg.TriggerThreshold[bizKey]; ok && n > 0 {
		return n
	}
	return m.cfg.DefaultTriggerThreshold
}

// Put 实现.
func (m *MemoryLayer) Put(_ context.Context, e *store.Event) ([]TriggerKey, error) {
	if e == nil {
		return nil, errors.New("nil event")
	}
	if len(e.Indexes) == 0 {
		return nil, nil
	}
	eventID := e.Service + ":" + e.Table + ":" + e.PK
	m.mu.Lock()
	defer m.mu.Unlock()
	var triggers []TriggerKey
	for bizKey, val := range e.Indexes {
		if val == "" {
			continue
		}
		bk := bizKey + ":" + val
		if m.buckets[bk] == nil {
			m.buckets[bk] = map[string]*store.Event{}
		}
		copyE := *e
		m.buckets[bk][eventID] = &copyE
		if len(m.buckets[bk]) >= m.triggerN(bizKey) {
			t := TriggerKey{BizKey: bizKey, Value: val}
			m.trigger = append(m.trigger, t)
			triggers = append(triggers, t)
		}
	}
	return triggers, nil
}

// Get 实现.
func (m *MemoryLayer) Get(_ context.Context, bizKey, val string) ([]*store.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bk := bizKey + ":" + val
	out := make([]*store.Event, 0, len(m.buckets[bk]))
	for _, e := range m.buckets[bk] {
		c := *e
		out = append(out, &c)
	}
	return out, nil
}

// Pop 实现 (非阻塞;timeout 忽略).
func (m *MemoryLayer) Pop(_ context.Context, count int, _ time.Duration) ([]TriggerKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if count <= 0 || len(m.trigger) == 0 {
		return nil, nil
	}
	n := count
	if n > len(m.trigger) {
		n = len(m.trigger)
	}
	out := append([]TriggerKey(nil), m.trigger[:n]...)
	m.trigger = m.trigger[n:]
	return out, nil
}

// AckMatch 实现.
func (m *MemoryLayer) AckMatch(_ context.Context, t TriggerKey, keepHistory bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bk := t.BizKey + ":" + t.Value
	if !keepHistory {
		delete(m.buckets, bk)
	}
	return nil
}

// Lock 实现.
func (m *MemoryLayer) Lock(_ context.Context, t TriggerKey) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := t.String()
	if m.locks[k] {
		return nil, false, nil
	}
	m.locks[k] = true
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.locks, k)
	}, true, nil
}

// Sweep no-op (内存版没真实过期机制).
func (m *MemoryLayer) Sweep(_ context.Context, _ time.Duration) (int, error) {
	return 0, nil
}

// Stats 实现.
func (m *MemoryLayer) Stats(_ context.Context) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{"trigger_queue_depth": len(m.trigger)}
	for k := range m.buckets {
		bizKey := strings.SplitN(k, ":", 2)[0]
		out["bucket_"+bizKey]++
	}
	return out, nil
}

// Close no-op.
func (m *MemoryLayer) Close() error { return nil }

// 编译期检查.
var _ Layer = (*MemoryLayer)(nil)

// Serialize 把 event 转 JSON (调试用).
func Serialize(e *store.Event) string {
	b, _ := json.Marshal(e)
	return string(b)
}

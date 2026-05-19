// Package repo — split-payment 仓储抽象 + 内存实现 (dev/test 用)。
// 生产换 MySQL: 见 mysql.go (按 graph_id / charge_id 分片)。

package repo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/xiongwp/split-payment/internal/domain"
)

// ErrNotFound ...
var ErrNotFound = errors.New("not found")

// ─── Graph repo ────────────────────────────────────────────────────────

// MemoryGraphRepo 内存版。
type MemoryGraphRepo struct {
	mu       sync.RWMutex
	byID     map[int64]*domain.Graph
	byKey    map[string]*domain.Graph
	nextID   int64
}

// NewMemoryGraphRepo 构造。
func NewMemoryGraphRepo() *MemoryGraphRepo {
	return &MemoryGraphRepo{byID: map[int64]*domain.Graph{}, byKey: map[string]*domain.Graph{}}
}

// Save 创建 or 更新; 用 key 当业务幂等键。
func (r *MemoryGraphRepo) Save(_ context.Context, g *domain.Graph) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.byKey[g.Key]; ok {
		// 升版本
		g.ID = existing.ID
		g.CreatedAt = existing.CreatedAt
		g.UpdatedAt = time.Now().UTC()
		r.byID[g.ID] = g
		r.byKey[g.Key] = g
		return g.ID, nil
	}
	r.nextID++
	g.ID = r.nextID
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now().UTC()
	}
	g.UpdatedAt = g.CreatedAt
	r.byID[g.ID] = g
	r.byKey[g.Key] = g
	return g.ID, nil
}

// GetByKey 按业务键查。
func (r *MemoryGraphRepo) GetByKey(_ context.Context, key string) (*domain.Graph, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.byKey[key]
	if !ok {
		return nil, ErrNotFound
	}
	return g, nil
}

// Delete soft-delete: status → "archived". key 不存在静默成功 (idempotent).
func (r *MemoryGraphRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.byKey[key]; ok {
		g.Status = "archived"
		g.UpdatedAt = time.Now().UTC()
	}
	return nil
}

// FindByTrigger 找所有 active 且 triggers 含此 event 的 graph。
func (r *MemoryGraphRepo) FindByTrigger(_ context.Context, event string) ([]*domain.Graph, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.Graph{}
	for _, g := range r.byID {
		if g.Status != "active" {
			continue
		}
		for _, t := range g.Spec.Triggers {
			if t.Event == event {
				out = append(out, g)
				break
			}
		}
	}
	return out, nil
}

// List 按状态列。
func (r *MemoryGraphRepo) List(_ context.Context, status string) ([]*domain.Graph, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.Graph{}
	for _, g := range r.byID {
		if status != "" && g.Status != status && status != "all" {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

// ─── Run repo ──────────────────────────────────────────────────────────

// MemoryRunRepo 内存版。
type MemoryRunRepo struct {
	mu     sync.RWMutex
	runs   map[int64]*domain.RunPlan
	byChrg map[string][]int64
	nextID int64
}

// NewMemoryRunRepo 构造。
func NewMemoryRunRepo() *MemoryRunRepo {
	return &MemoryRunRepo{runs: map[int64]*domain.RunPlan{}, byChrg: map[string][]int64{}}
}

// Save ...
func (r *MemoryRunRepo) Save(_ context.Context, p *domain.RunPlan) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	p.ID = r.nextID
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	r.runs[p.ID] = p
	r.byChrg[p.ChargeID] = append(r.byChrg[p.ChargeID], p.ID)
	return p.ID, nil
}

// Update ...
func (r *MemoryRunRepo) Update(_ context.Context, p *domain.RunPlan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runs[p.ID]; !ok {
		return ErrNotFound
	}
	r.runs[p.ID] = p
	return nil
}

// GetByCharge 拉同一 charge 关联的所有 run plan。
func (r *MemoryRunRepo) GetByCharge(_ context.Context, chargeID string) ([]*domain.RunPlan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids, ok := r.byChrg[chargeID]
	if !ok {
		return nil, nil
	}
	out := make([]*domain.RunPlan, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.runs[id])
	}
	return out, nil
}

// ListExpiredHolds SP-AC-7 PH3-7 内存模式: 永远返空 (dev 不真跑 hold).
func (r *MemoryRunRepo) ListExpiredHolds(_ context.Context, _ time.Time, _ int) ([]*domain.RunPlan, error) {
	return nil, nil
}

// MarkHoldReleased SP-AC-7 PH3-7 内存模式 noop.
func (r *MemoryRunRepo) MarkHoldReleased(_ context.Context, _ int64) error { return nil }

// Searchable 支持按 trigger event 模糊过滤 (admin 后台查询用)。
func (r *MemoryRunRepo) Search(_ context.Context, eventLike string, limit int) ([]*domain.RunPlan, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.RunPlan{}
	for _, p := range r.runs {
		if eventLike != "" && !strings.Contains(p.TriggerEvent, eventLike) {
			continue
		}
		out = append(out, p)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

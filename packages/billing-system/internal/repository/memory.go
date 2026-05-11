// Package repository — 数据存储抽象 + 实现。
//
// 提供 2 套实现:
//   MemoryRepo  线程安全 in-memory store，单元测试 / dev 起步用
//   MySQLRepo   生产用，按 merchant_id hash 分 10 库（沿用项目 sharded 风格）
//
// 接口设计参考 feecalc.Repository + statement.Repository 取并集。

package repository

import (
	"context"
	"sort"
	"sync"
	"time"

	"reconcile-system/packages/billing-system/internal/domain"
)

// MemoryRepo 内存实现 — 数据不持久化。
type MemoryRepo struct {
	mu          sync.RWMutex
	feeEvents   map[int64]*domain.FeeEvent
	statements  map[int64]*domain.Statement
	nextEventID int64
	nextStmtID  int64
}

func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		feeEvents:  map[int64]*domain.FeeEvent{},
		statements: map[int64]*domain.Statement{},
	}
}

// ─── feecalc.Repository ────────────────────────────────────────────

func (r *MemoryRepo) SaveFeeEvent(ctx context.Context, ev *domain.FeeEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextEventID++
	ev.ID = r.nextEventID
	r.feeEvents[ev.ID] = ev
	return nil
}

func (r *MemoryRepo) LoadOriginalCharge(ctx context.Context, merchantID, refID string) (*domain.FeeEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.feeEvents {
		if e.MerchantID == merchantID && e.RefID == refID && e.EventType == domain.EventCharge {
			return e, nil
		}
	}
	return nil, nil
}

func (r *MemoryRepo) ExistsByRef(ctx context.Context, merchantID, refID string, eventType domain.EventType) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.feeEvents {
		if e.MerchantID == merchantID && e.RefID == refID && e.EventType == eventType {
			return true, nil
		}
	}
	return false, nil
}

// ─── statement.Repository ──────────────────────────────────────────

func (r *MemoryRepo) ListPendingEvents(ctx context.Context, merchantID string, from, to time.Time) ([]*domain.FeeEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*domain.FeeEvent
	for _, e := range r.feeEvents {
		if e.MerchantID != merchantID || e.Status != domain.StatusPending {
			continue
		}
		if e.OccurredAt.Before(from) || !e.OccurredAt.Before(to) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

func (r *MemoryRepo) SaveStatement(ctx context.Context, s *domain.Statement) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextStmtID++
	s.ID = r.nextStmtID
	r.statements[s.ID] = s
	return s.ID, nil
}

func (r *MemoryRepo) MarkEventsSettled(ctx context.Context, eventIDs []int64, statementID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range eventIDs {
		if e, ok := r.feeEvents[id]; ok {
			e.Status = domain.StatusSettled
			e.StatementID = statementID
		}
	}
	return nil
}

func (r *MemoryRepo) ListMerchantIDs(ctx context.Context, from, to time.Time) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, e := range r.feeEvents {
		if e.OccurredAt.Before(from) || !e.OccurredAt.Before(to) {
			continue
		}
		if e.Status != domain.StatusPending {
			continue
		}
		seen[e.MerchantID] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

// ─── 商户后台查询接口 ────────────────────────────────────────────────

// ListStatementsByMerchant 拉商户最近 N 期账单（admin web 用）。
func (r *MemoryRepo) ListStatementsByMerchant(ctx context.Context, merchantID string, limit int) ([]*domain.Statement, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*domain.Statement
	for _, s := range r.statements {
		if s.MerchantID == merchantID {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PeriodStart.After(out[j].PeriodStart) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetStatement 拉单期账单。
func (r *MemoryRepo) GetStatement(ctx context.Context, id int64) (*domain.Statement, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.statements[id], nil
}

// ListStatementEvents 拉某账单下所有 fee_event（账单明细页用）。
func (r *MemoryRepo) ListStatementEvents(ctx context.Context, statementID int64) ([]*domain.FeeEvent, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*domain.FeeEvent
	for _, e := range r.feeEvents {
		if e.StatementID == statementID {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

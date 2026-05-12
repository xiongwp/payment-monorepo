// Package store — AML 名单 + 命中记录持久化.
//
// 接口设计成 in-memory + mysql 双实现; dev 用 memory, 生产用 mysql.
// 名单 70w+ 条全部加载内存索引 (按 first-letter / source / country) → fast cand
// 退化路径: 索引未命中也支持全表扫 (告警).

package store

import (
	"errors"
	"sync"
	"time"

	"reconcile-system/packages/aml-screening/internal/domain"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Store 总接口
type Store interface {
	// 名单条目 CRUD
	UpsertEntry(e domain.ListEntry) error
	GetEntry(source domain.ListSource, id string) (domain.ListEntry, error)
	// 检索候选 - 先按 first-letter 粗筛, 然后 nationality / source filter
	Candidates(prefix string, sources []domain.ListSource, countryISO string) ([]domain.ListEntry, error)

	// 命中记录
	SaveResult(res domain.ScreenResult, req domain.ScreenRequest) error
	GetResult(requestID string) (domain.ScreenResult, error)
	UpdateHitState(hitID string, decision domain.HitResolution) error
	ListPendingHits(limit, offset int) ([]domain.HitInfo, error)

	// 维护
	EntryCount(source domain.ListSource) (int, error)
	PurgeStaleEntries(source domain.ListSource, before time.Time) (int, error)
}

// ─────────────────────────────────────────────────────────
// Memory 实现
// ─────────────────────────────────────────────────────────

type MemStore struct {
	mu       sync.RWMutex
	entries  map[string]domain.ListEntry          // key = source|id
	byPrefix map[string][]string                  // first-letter → []entryKey (粗排索引)
	results  map[string]storedResult              // requestID → result
	hits     map[string]*domain.HitInfo           // hitID → 引用; 跟 results 共享底层
}

type storedResult struct {
	req domain.ScreenRequest
	res domain.ScreenResult
}

func NewMemStore() *MemStore {
	return &MemStore{
		entries:  make(map[string]domain.ListEntry),
		byPrefix: make(map[string][]string),
		results:  make(map[string]storedResult),
		hits:     make(map[string]*domain.HitInfo),
	}
}

func entryKey(src domain.ListSource, id string) string { return string(src) + "|" + id }

func (s *MemStore) UpsertEntry(e domain.ListEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := entryKey(e.Source, e.ID)
	_, existed := s.entries[k]
	s.entries[k] = e
	if !existed {
		prefix := ""
		if e.PrimaryName != "" {
			prefix = string([]rune(e.PrimaryName)[0])
		}
		s.byPrefix[prefix] = append(s.byPrefix[prefix], k)
	}
	return nil
}

func (s *MemStore) GetEntry(src domain.ListSource, id string) (domain.ListEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entries[entryKey(src, id)]
	if !ok {
		return domain.ListEntry{}, ErrNotFound
	}
	return e, nil
}

// Candidates 候选粗筛 — 用 first-letter 索引 + 源 + 国籍过滤.
func (s *MemStore) Candidates(prefix string, sources []domain.ListSource, countryISO string) ([]domain.ListEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	srcSet := map[domain.ListSource]bool{}
	for _, src := range sources {
		srcSet[src] = true
	}
	// 收候选 key 集合
	keys := s.byPrefix[prefix]
	out := make([]domain.ListEntry, 0, len(keys))
	for _, k := range keys {
		e := s.entries[k]
		if len(srcSet) > 0 && !srcSet[e.Source] {
			continue
		}
		// 国籍过滤 — 弱过滤: 命名实体可能多国籍, 任一命中保留
		if countryISO != "" && len(e.Nationality) > 0 {
			match := false
			for _, n := range e.Nationality {
				if n == countryISO {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *MemStore) SaveResult(res domain.ScreenResult, req domain.ScreenRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[res.RequestID] = storedResult{req: req, res: res}
	for i := range res.Hits {
		h := &res.Hits[i]
		s.hits[h.HitID] = h
	}
	return nil
}

func (s *MemStore) GetResult(requestID string) (domain.ScreenResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.results[requestID]
	if !ok {
		return domain.ScreenResult{}, ErrNotFound
	}
	return r.res, nil
}

func (s *MemStore) UpdateHitState(hitID string, decision domain.HitResolution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hits[hitID]
	if !ok {
		return ErrNotFound
	}
	h.State = decision.Decision
	return nil
}

func (s *MemStore) ListPendingHits(limit, offset int) ([]domain.HitInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := make([]domain.HitInfo, 0)
	for _, h := range s.hits {
		if h.State == domain.HitPendingReview {
			all = append(all, *h)
		}
	}
	// 简单 slicing — 真实应该排序
	if offset >= len(all) {
		return nil, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

func (s *MemStore) EntryCount(src domain.ListSource) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.entries {
		if e.Source == src {
			n++
		}
	}
	return n, nil
}

func (s *MemStore) PurgeStaleEntries(src domain.ListSource, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	purged := 0
	for k, e := range s.entries {
		if e.Source == src && e.UpdatedAt.Before(before) {
			delete(s.entries, k)
			purged++
		}
	}
	// 索引懒回收 - 生产用 mysql impl 写 SQL DELETE
	return purged, nil
}

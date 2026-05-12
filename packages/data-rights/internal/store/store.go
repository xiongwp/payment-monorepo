// Package store — data-rights 持久化.

package store

import (
	"errors"
	"sort"
	"sync"
	"time"

	"reconcile-system/packages/data-rights/internal/domain"
)

var ErrNotFound = errors.New("not found")

type Store interface {
	SaveRequest(req domain.Request) error
	GetRequest(id string) (domain.Request, error)
	ListRequests(filter ListFilter) ([]domain.Request, error)
	UpdateServiceStatus(reqID string, st domain.ServiceStatus) error
	UpdateState(reqID string, state domain.State, by string) error
	SetExport(reqID, url, sha string) error
}

type ListFilter struct {
	State    domain.State
	Type     domain.RequestType
	Overdue  bool // deadline_at < now & state not in fulfilled/rejected
	Limit    int
	Offset   int
}

type MemStore struct {
	mu   sync.RWMutex
	reqs map[string]domain.Request
}

func NewMemStore() *MemStore {
	return &MemStore{reqs: make(map[string]domain.Request)}
}

func (s *MemStore) SaveRequest(req domain.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs[req.RequestID] = req
	return nil
}

func (s *MemStore) GetRequest(id string) (domain.Request, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.reqs[id]
	if !ok {
		return domain.Request{}, ErrNotFound
	}
	return r, nil
}

func (s *MemStore) ListRequests(f ListFilter) ([]domain.Request, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := make([]domain.Request, 0, len(s.reqs))
	now := time.Now().UTC()
	for _, r := range s.reqs {
		if f.State != "" && r.State != f.State {
			continue
		}
		if f.Type != "" && r.Type != f.Type {
			continue
		}
		if f.Overdue {
			if r.State == domain.StateFulfilled || r.State == domain.StateRejected {
				continue
			}
			if r.DeadlineAt.IsZero() || r.DeadlineAt.After(now) {
				continue
			}
		}
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].SubmittedAt.After(all[j].SubmittedAt)
	})
	if f.Offset >= len(all) {
		return nil, nil
	}
	end := f.Offset + f.Limit
	if f.Limit == 0 || end > len(all) {
		end = len(all)
	}
	return all[f.Offset:end], nil
}

func (s *MemStore) UpdateServiceStatus(reqID string, st domain.ServiceStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reqs[reqID]
	if !ok {
		return ErrNotFound
	}
	updated := false
	for i, existing := range r.ServiceStatuses {
		if existing.Service == st.Service {
			r.ServiceStatuses[i] = st
			updated = true
			break
		}
	}
	if !updated {
		r.ServiceStatuses = append(r.ServiceStatuses, st)
	}
	s.reqs[reqID] = r
	return nil
}

func (s *MemStore) UpdateState(reqID string, state domain.State, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reqs[reqID]
	if !ok {
		return ErrNotFound
	}
	r.State = state
	switch state {
	case domain.StateApproved:
		r.ApprovedAt = time.Now().UTC()
		r.ApprovedBy = by
	case domain.StateFulfilled:
		r.FulfilledAt = time.Now().UTC()
	case domain.StateRejected:
		r.RejectReason = by // by = reason 在 reject 时
	}
	s.reqs[reqID] = r
	return nil
}

func (s *MemStore) SetExport(reqID, url, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reqs[reqID]
	if !ok {
		return ErrNotFound
	}
	r.ExportURL = url
	r.ExportSHA256 = sha
	s.reqs[reqID] = r
	return nil
}

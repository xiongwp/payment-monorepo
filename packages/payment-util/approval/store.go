package approval

import (
	"errors"
	"sort"
	"sync"
)

var (
	ErrNotFound          = errors.New("approval: not found")
	ErrAlreadyDecided    = errors.New("approval: already decided (approved/rejected/executed)")
	ErrSelfApproval      = errors.New("approval: requester cannot self-approve")
	ErrDuplicateReviewer = errors.New("approval: reviewer already approved")
	ErrInvalidState      = errors.New("approval: invalid state transition")
)

type Store interface {
	Create(a Action) error
	Get(id string) (Action, error)
	Update(a Action) error
	List(filter ListFilter) ([]Action, error)
}

type ListFilter struct {
	State      State
	Type       string
	Requester  string
	Reviewer   string // 包含此 reviewer 审过的
	Limit      int
	Offset     int
}

type MemStore struct {
	mu      sync.RWMutex
	actions map[string]Action
}

func NewMemStore() *MemStore {
	return &MemStore{actions: make(map[string]Action)}
}

func (s *MemStore) Create(a Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.actions[a.ID]; ok {
		return errors.New("approval: duplicate ID")
	}
	s.actions[a.ID] = a
	return nil
}

func (s *MemStore) Get(id string) (Action, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.actions[id]
	if !ok {
		return Action{}, ErrNotFound
	}
	return a, nil
}

func (s *MemStore) Update(a Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.actions[a.ID]; !ok {
		return ErrNotFound
	}
	s.actions[a.ID] = a
	return nil
}

func (s *MemStore) List(f ListFilter) ([]Action, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := make([]Action, 0, len(s.actions))
	for _, a := range s.actions {
		if f.State != "" && a.State != f.State {
			continue
		}
		if f.Type != "" && a.Type != f.Type {
			continue
		}
		if f.Requester != "" && a.Requester != f.Requester {
			continue
		}
		if f.Reviewer != "" {
			has := false
			for _, ap := range a.Approvals {
				if ap.Reviewer == f.Reviewer {
					has = true
					break
				}
			}
			if !has {
				continue
			}
		}
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].RequesterAt.After(all[j].RequesterAt)
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

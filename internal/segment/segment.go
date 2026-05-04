package segment

import (
	"sync"
)

type Segment struct {
	mu      sync.Mutex
	current int64
	max     int64
}

func New(start, step int64) *Segment {
	return &Segment{
		current: start,
		max:     start + step,
	}
}

func (s *Segment) Next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current >= s.max {
		return -1
	}
	s.current++
	return s.current
}

package segment

import (
	"log"
	"sync"

	"gorm.io/gorm"
)

type Buffer struct {
	mu      sync.Mutex
	Current int64
	max     int64
	next    *Buffer
	DB      *gorm.DB
}

func (b *Buffer) Next() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.Current >= b.max {
		if b.next != nil {
			*b = *b.next
			go b.Load()
		} else {
			return -1
		}
	}

	b.Current++
	return b.Current
}

func (b *Buffer) Load() {
	id, err := Fetch(b.DB)
	if err != nil {
		log.Fatalf("fetch segment failed: %v", err)
	}
	b.Current = id
}

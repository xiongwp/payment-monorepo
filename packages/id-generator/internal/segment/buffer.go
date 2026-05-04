package segment

import (
	"context"
	"log"
	"sync"

	"gorm.io/gorm"

	"github.com/xiongwp/payment-util/shadow"
)

// Buffer 单一号段 buffer。每个 (主 / 影子) 路径各持有一个独立 Buffer，
// 互不影响（号段空间独立、并发锁独立）。
//
// IsShadow=true 时所有 DB 操作走 id_segment_shadow 表。
type Buffer struct {
	mu       sync.Mutex
	Current  int64
	max      int64
	next     *Buffer
	DB       *gorm.DB
	IsShadow bool // 决定 Load() 时去主表还是影子表 Fetch
}

// NewMainBuffer / NewShadowBuffer 显式构造，避免调用方写错 IsShadow 字段。
func NewMainBuffer(db *gorm.DB) *Buffer   { return &Buffer{DB: db, IsShadow: false} }
func NewShadowBuffer(db *gorm.DB) *Buffer { return &Buffer{DB: db, IsShadow: true} }

// ctx 返回 Buffer 自身视角的 ctx（仅用于 segmentTable 路由）。
func (b *Buffer) ctx() context.Context {
	bg := context.Background()
	if b.IsShadow {
		return shadow.WithShadow(bg, true)
	}
	return bg
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
	id, err := Fetch(b.ctx(), b.DB)
	if err != nil {
		log.Fatalf("fetch segment failed (shadow=%v): %v", b.IsShadow, err)
	}
	b.Current = id
}

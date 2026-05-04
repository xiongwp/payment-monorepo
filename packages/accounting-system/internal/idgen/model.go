package idgen

import (
	"sync"
	"sync/atomic"
	"time"
)

// LeafAlloc 号段分配表映射（对应 leaf_alloc 表）
type LeafAlloc struct {
	BizTag      string    `gorm:"column:biz_tag;primaryKey"`
	MaxID       int64     `gorm:"column:max_id"`
	Step        int       `gorm:"column:step"`
	Description *string   `gorm:"column:description"`
	UpdateTime  time.Time `gorm:"column:update_time;autoUpdateTime"`
}

func (LeafAlloc) TableName() string { return "leaf_alloc" }

// ─── Segment：单个号段 ──────────────────────────────────────────────────────────

// Segment 代表从数据库分配到的一个号段 [cur, max)。
// cur 使用 atomic int64，允许无锁自增；max 为只读，初始化后不变。
type Segment struct {
	cur  atomic.Int64
	max  int64
	step int
}

func newSegment(start int64, step int) *Segment {
	s := &Segment{max: start + int64(step), step: step}
	s.cur.Store(start)
	return s
}

// next 返回下一个 ID；若号段已用完则返回 -1。
func (s *Segment) next() int64 {
	v := s.cur.Add(1) - 1
	if v >= s.max {
		return -1
	}
	return v
}

func (s *Segment) remaining() int64 {
	r := s.max - s.cur.Load()
	if r < 0 {
		return 0
	}
	return r
}

func (s *Segment) total() int64    { return int64(s.step) }
func (s *Segment) exhausted() bool { return s.cur.Load() >= s.max }

// ─── SegmentBuffer：双 Buffer ──────────────────────────────────────────────────

// segmentBuffer 为单个 biz_tag 维护双号段缓冲，减少数据库访问频率。
type segmentBuffer struct {
	mu         sync.Mutex
	bizTag     string
	segments   [2]*Segment
	current    int
	loadFactor float64
	nextReady  bool
	loading    bool
}

func newSegmentBuffer(bizTag string, loadFactor float64) *segmentBuffer {
	return &segmentBuffer{bizTag: bizTag, loadFactor: loadFactor}
}

func (b *segmentBuffer) currentSeg() *Segment { return b.segments[b.current] }
func (b *segmentBuffer) nextIdx() int          { return 1 - b.current }

func (b *segmentBuffer) shouldPreload() bool {
	if b.nextReady || b.loading {
		return false
	}
	seg := b.currentSeg()
	consumed := float64(seg.total()-seg.remaining()) / float64(seg.total())
	return consumed >= b.loadFactor
}

func (b *segmentBuffer) markLoading()  { b.loading = true }
func (b *segmentBuffer) cancelLoading() { b.loading = false }
func (b *segmentBuffer) nextReady_() bool { return b.nextReady }
func (b *segmentBuffer) isLoading() bool  { return b.loading }

func (b *segmentBuffer) initialize(seg *Segment) {
	b.segments[0] = seg
	b.current = 0
	b.nextReady = false
	b.loading = false
}

func (b *segmentBuffer) setNextSegment(seg *Segment) {
	b.segments[b.nextIdx()] = seg
	b.nextReady = true
	b.loading = false
}

func (b *segmentBuffer) switchBuffer() {
	b.current = b.nextIdx()
	b.nextReady = false
}

func (b *segmentBuffer) lock()   { b.mu.Lock() }
func (b *segmentBuffer) unlock() { b.mu.Unlock() }

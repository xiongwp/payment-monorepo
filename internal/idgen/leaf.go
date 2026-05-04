// Package idgen 提供 Leaf Segment 模式的号段 ID 生成器。
//
// 用 order_meta.leaf_alloc 表持久化各 biz_tag 的 max_id；
// 进程内双 buffer，正常路径无 DB IO，号段消耗到 90% 异步预拉下一段。
package idgen

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 业务标签
const (
	BizTagPaymentIntent    = "order.payment_intent"
	BizTagCharge           = "order.charge"
	BizTagRefund           = "order.refund"
	BizTagInboundWebhook   = "order.inbound_webhook"
	BizTagLedgerTxn        = "order.ledger_txn"
	BizTagDispute          = "order.dispute"
	BizTagAccountingOutbox = "order.accounting_outbox"
)

const (
	defaultStep      = 100_000
	defaultInitMaxID = 1_000_000
)

// LeafAlloc leaf_alloc 表
type LeafAlloc struct {
	BizTag      string    `gorm:"column:biz_tag;primaryKey"`
	MaxID       int64     `gorm:"column:max_id"`
	Step        int       `gorm:"column:step"`
	Description *string   `gorm:"column:description"`
	UpdateTime  time.Time `gorm:"column:update_time;autoUpdateTime"`
}

// TableName 表名
func (LeafAlloc) TableName() string { return "leaf_alloc" }

// IDGenerator 号段 ID 生成器
type IDGenerator interface {
	NextID(ctx context.Context, bizTag string) (int64, error)
	Register(ctx context.Context, bizTag string, initMaxID int64, step int, desc string) error
	Preload(ctx context.Context) error
}

// segment 单个号段 [cur, max)
type segment struct {
	cur  atomic.Int64
	max  int64
	step int
}

func newSegment(start int64, step int) *segment {
	s := &segment{max: start + int64(step), step: step}
	s.cur.Store(start)
	return s
}
func (s *segment) next() int64 {
	v := s.cur.Add(1) - 1
	if v >= s.max {
		return -1
	}
	return v
}
func (s *segment) consumed() float64 {
	used := s.max - s.cur.Load()
	if used < 0 {
		used = 0
	}
	return 1 - float64(used)/float64(s.step)
}

// buffer 双号段 buffer
type buffer struct {
	mu        sync.Mutex
	bizTag    string
	segments  [2]*segment
	current   int
	loadFactor float64
	nextReady bool
	loading   bool
}

func (b *buffer) cur() *segment { return b.segments[b.current] }

// segmentRepo
type segmentRepo struct{ db *gorm.DB }

func (r *segmentRepo) get(ctx context.Context, biz string) (*LeafAlloc, error) {
	var a LeafAlloc
	err := r.db.WithContext(ctx).Where("biz_tag = ?", biz).First(&a).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &a, err
}
func (r *segmentRepo) listAll(ctx context.Context) ([]LeafAlloc, error) {
	var out []LeafAlloc
	err := r.db.WithContext(ctx).Find(&out).Error
	return out, err
}
func (r *segmentRepo) register(ctx context.Context, a *LeafAlloc) error {
	return r.db.WithContext(ctx).Where("biz_tag = ?", a.BizTag).FirstOrCreate(a).Error
}
func (r *segmentRepo) advance(ctx context.Context, biz string) (*LeafAlloc, error) {
	var out LeafAlloc
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&LeafAlloc{}).Where("biz_tag = ?", biz).
			Update("max_id", gorm.Expr("max_id + step")).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("biz_tag = ?", biz).First(&out).Error
	})
	return &out, err
}

type generator struct {
	repo       *segmentRepo
	loadFactor float64
	buffers    sync.Map // map[string]*buffer
	logger     *zap.Logger
}

// New 用 metaDB 构造 IDGenerator，并预注册三个 biz_tag。
func New(metaDB *gorm.DB, logger *zap.Logger) (IDGenerator, error) {
	g := &generator{repo: &segmentRepo{db: metaDB}, loadFactor: 0.9, logger: logger}
	ctx := context.Background()
	for _, biz := range []struct{ name, desc string }{
		{BizTagPaymentIntent, "PaymentIntent id"},
		{BizTagCharge, "Charge id"},
		{BizTagRefund, "Refund id"},
		{BizTagInboundWebhook, "InboundWebhook id"},
		{BizTagLedgerTxn, "Ledger transaction id"},
		{BizTagDispute, "Dispute id"},
		{BizTagAccountingOutbox, "AccountingOutbox id"},
	} {
		if err := g.Register(ctx, biz.name, defaultInitMaxID, defaultStep, biz.desc); err != nil {
			return nil, err
		}
	}
	if err := g.Preload(ctx); err != nil {
		logger.Warn("idgen preload failed", zap.Error(err))
	}
	return g, nil
}

func (g *generator) Register(ctx context.Context, biz string, initMaxID int64, step int, desc string) error {
	if biz == "" {
		return fmt.Errorf("biz_tag empty")
	}
	if step <= 0 {
		step = defaultStep
	}
	if initMaxID <= 0 {
		initMaxID = defaultInitMaxID
	}
	a := &LeafAlloc{BizTag: biz, MaxID: initMaxID, Step: step}
	if desc != "" {
		a.Description = &desc
	}
	return g.repo.register(ctx, a)
}

func (g *generator) Preload(ctx context.Context) error {
	allocs, err := g.repo.listAll(ctx)
	if err != nil {
		return err
	}
	for _, a := range allocs {
		buf := g.bufferFor(a.BizTag)
		_ = g.loadInto(ctx, a.BizTag, buf)
	}
	return nil
}

func (g *generator) NextID(ctx context.Context, biz string) (int64, error) {
	buf, err := g.ensure(ctx, biz)
	if err != nil {
		return 0, err
	}
	for {
		buf.mu.Lock()
		seg := buf.cur()
		id := seg.next()
		if id >= 0 {
			if !buf.nextReady && !buf.loading && seg.consumed() >= buf.loadFactor {
				buf.loading = true
				go func() {
					_ = g.loadInto(context.Background(), biz, buf)
				}()
			}
			buf.mu.Unlock()
			return id, nil
		}
		// current 耗尽
		if buf.nextReady {
			buf.current = 1 - buf.current
			buf.nextReady = false
			buf.mu.Unlock()
			continue
		}
		if !buf.loading {
			buf.loading = true
			buf.mu.Unlock()
			if err := g.loadInto(ctx, biz, buf); err != nil {
				return 0, err
			}
			continue
		}
		buf.mu.Unlock()
		runtime.Gosched()
	}
}

func (g *generator) bufferFor(biz string) *buffer {
	if v, ok := g.buffers.Load(biz); ok {
		return v.(*buffer)
	}
	nb := &buffer{bizTag: biz, loadFactor: g.loadFactor}
	v, _ := g.buffers.LoadOrStore(biz, nb)
	return v.(*buffer)
}

func (g *generator) ensure(ctx context.Context, biz string) (*buffer, error) {
	buf := g.bufferFor(biz)
	buf.mu.Lock()
	if buf.cur() != nil {
		buf.mu.Unlock()
		return buf, nil
	}
	buf.mu.Unlock()
	a, err := g.repo.get(ctx, biz)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, fmt.Errorf("biz_tag not registered: %s", biz)
	}
	if err := g.loadInto(ctx, biz, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func (g *generator) loadInto(ctx context.Context, biz string, buf *buffer) error {
	a, err := g.repo.advance(ctx, biz)
	if err != nil {
		return err
	}
	start := a.MaxID - int64(a.Step)
	seg := newSegment(start, a.Step)
	buf.mu.Lock()
	defer buf.mu.Unlock()
	if buf.cur() == nil {
		buf.segments[0] = seg
		buf.current = 0
	} else {
		buf.segments[1-buf.current] = seg
		buf.nextReady = true
	}
	buf.loading = false
	return nil
}

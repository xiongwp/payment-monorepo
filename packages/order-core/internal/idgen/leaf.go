// Package idgen 提供 Leaf Segment 模式的号段 ID 生成器。
//
// 用 order_meta.leaf_alloc 表持久化各 biz_tag 的 max_id；
// 进程内双 buffer，正常路径无 DB IO，号段消耗到 90% 异步预拉下一段。
//
// shadow 支持：
//   - 同一 biz_tag 在主表 leaf_alloc 和影子表 leaf_alloc_shadow 各注册一份
//   - generator 内部以 (biz_tag, isShadow) 为 key 维护两套独立 buffer
//   - NextID(ctx, biz) 根据 shadow.IsShadow(ctx) 选 buffer + 选表名
//     主流量号段不被压测消耗；两套号段互不感知
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

	"github.com/xiongwp/order-core/internal/shadow"
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

// 主表 / 影子表名常量
const (
	leafAllocBase = "leaf_alloc"
)

// TableName gorm 默认 fallback 主表；shadow 路径必须显式 .Table(leafTable(ctx))。
func (LeafAlloc) TableName() string { return leafAllocBase }

// leafTable 按 ctx 解析号段表名（leaf_alloc / leaf_alloc_shadow）。
func leafTable(ctx context.Context) string {
	return shadow.TableName(ctx, leafAllocBase)
}

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

// segmentRepo 所有方法都按 ctx 解析表名（主 / 影子）。
type segmentRepo struct{ db *gorm.DB }

func (r *segmentRepo) get(ctx context.Context, biz string) (*LeafAlloc, error) {
	var a LeafAlloc
	err := r.db.WithContext(ctx).Table(leafTable(ctx)).Where("biz_tag = ?", biz).First(&a).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &a, err
}
func (r *segmentRepo) listAll(ctx context.Context) ([]LeafAlloc, error) {
	var out []LeafAlloc
	err := r.db.WithContext(ctx).Table(leafTable(ctx)).Find(&out).Error
	return out, err
}
func (r *segmentRepo) register(ctx context.Context, a *LeafAlloc) error {
	// FirstOrCreate 用 struct 写入需要 Table 显式：否则 gorm 用 LeafAlloc.TableName() = "leaf_alloc" 即主表。
	tbl := leafTable(ctx)
	var existing LeafAlloc
	err := r.db.WithContext(ctx).Table(tbl).Where("biz_tag = ?", a.BizTag).First(&existing).Error
	if err == nil {
		return nil // 已存在，幂等
	}
	if err != gorm.ErrRecordNotFound {
		return err
	}
	return r.db.WithContext(ctx).Table(tbl).Create(a).Error
}
func (r *segmentRepo) advance(ctx context.Context, biz string) (*LeafAlloc, error) {
	tbl := leafTable(ctx)
	var out LeafAlloc
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Table(tbl).Where("biz_tag = ?", biz).
			Update("max_id", gorm.Expr("max_id + step")).Error; err != nil {
			return err
		}
		return tx.Table(tbl).Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("biz_tag = ?", biz).First(&out).Error
	})
	return &out, err
}

type generator struct {
	repo       *segmentRepo
	loadFactor float64
	// buffers key = effectiveBufferKey(bizTag, isShadow)，主和影子分别独立 buffer
	buffers sync.Map // map[string]*buffer
	logger  *zap.Logger
}

// effectiveBufferKey 把 (bizTag, isShadow) 折成单一字符串作 sync.Map key。
// 主流量 → biz；影子 → "biz::shadow"。
func effectiveBufferKey(biz string, isShadow bool) string {
	if isShadow {
		return biz + "::shadow"
	}
	return biz
}

// New 用 metaDB 构造 IDGenerator，并预注册 7 个 biz_tag 到 leaf_alloc 主表 + 影子表。
// 主和影子号段独立递增。新部署 + init_shadow.sql 没跑过时，影子表的 register 会
// 因 leaf_alloc_shadow 不存在而报错；ApplyMetaShadowTables 启动期兜底建表，
// register 这一步走在它之后即可。
func New(metaDB *gorm.DB, logger *zap.Logger) (IDGenerator, error) {
	g := &generator{repo: &segmentRepo{db: metaDB}, loadFactor: 0.9, logger: logger}
	ctx := context.Background()
	bizTags := []struct{ name, desc string }{
		{BizTagPaymentIntent, "PaymentIntent id"},
		{BizTagCharge, "Charge id"},
		{BizTagRefund, "Refund id"},
		{BizTagInboundWebhook, "InboundWebhook id"},
		{BizTagLedgerTxn, "Ledger transaction id"},
		{BizTagDispute, "Dispute id"},
		{BizTagAccountingOutbox, "AccountingOutbox id"},
	}
	// 主表注册
	for _, biz := range bizTags {
		if err := g.Register(ctx, biz.name, defaultInitMaxID, defaultStep, biz.desc); err != nil {
			return nil, err
		}
	}
	// 影子表注册（leaf_alloc_shadow 由 ApplyMetaShadowTables 兜底建好；如果没建则
	// register 失败，warn 但不阻塞 — 主流量仍可用）。
	shadowCtx := shadow.WithShadow(ctx, true)
	for _, biz := range bizTags {
		if err := g.Register(shadowCtx, biz.name, defaultInitMaxID, defaultStep, biz.desc); err != nil {
			logger.Warn("idgen shadow register failed (shadow traffic will fail until leaf_alloc_shadow is provisioned)",
				zap.String("biz_tag", biz.name), zap.Error(err))
			break
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

// Preload 从主表 + 影子表分别 listAll，给每个 (biz_tag, shadow) buffer 都暖启一段。
func (g *generator) Preload(ctx context.Context) error {
	for _, isShadow := range []bool{false, true} {
		c := ctx
		if isShadow {
			c = shadow.WithShadow(ctx, true)
		}
		allocs, err := g.repo.listAll(c)
		if err != nil {
			if isShadow {
				// 影子表未配置时这里失败，warn 不阻塞主流量
				if g.logger != nil {
					g.logger.Warn("idgen shadow preload failed (skipping)", zap.Error(err))
				}
				continue
			}
			return err
		}
		for _, a := range allocs {
			buf := g.bufferFor(a.BizTag, isShadow)
			_ = g.loadInto(c, a.BizTag, buf)
		}
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
				// 异步预拉下一段：用 background ctx 脱离调用方 cancel，但保留 shadow flag。
				asyncCtx := shadow.WithShadow(context.Background(), shadow.IsShadow(ctx))
				go func() {
					_ = g.loadInto(asyncCtx, biz, buf)
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

// bufferFor 按 (biz, isShadow) 维度返回独立 buffer。
func (g *generator) bufferFor(biz string, isShadow bool) *buffer {
	key := effectiveBufferKey(biz, isShadow)
	if v, ok := g.buffers.Load(key); ok {
		return v.(*buffer)
	}
	nb := &buffer{bizTag: biz, loadFactor: g.loadFactor}
	v, _ := g.buffers.LoadOrStore(key, nb)
	return v.(*buffer)
}

func (g *generator) ensure(ctx context.Context, biz string) (*buffer, error) {
	isShadow := shadow.IsShadow(ctx)
	buf := g.bufferFor(biz, isShadow)
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
		return nil, fmt.Errorf("biz_tag not registered: %s (shadow=%v)", biz, isShadow)
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

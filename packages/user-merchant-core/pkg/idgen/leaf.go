// Package idgen 提供通用 Leaf Segment 号段 ID 生成器。
//
// 业务标签（biz_tag）由调用方在启动时通过 Register 注入；本包不内置任何
// service 专属常量 —— 可直接 replace 到 order-core / user-merchant-core 等。
//
// 持久化表名约定 `leaf_alloc`：
//   CREATE TABLE leaf_alloc (biz_tag VARCHAR(128) PK, max_id BIGINT, step INT, ...)
package idgen

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// leafTable 按 ctx 解析号段表名（leaf_alloc / leaf_alloc_shadow）。
func leafTable(ctx context.Context) string {
	return shadow.TableName(ctx, leafAllocBase)
}

// effectiveBufferKey 把 (bizTag, isShadow) 折成单一 buffer key；
// 主流量号段和影子号段独立维护，互不影响。
func effectiveBufferKey(biz string, isShadow bool) string {
	if isShadow {
		return biz + "::shadow"
	}
	return biz
}

const (
	defaultStep      = 100_000
	defaultInitMaxID = 1_000_000
)

// 主 / 影子号段表名常量（pkg/idgen 是通用包，不含具体 biz_tag）
const leafAllocBase = "leaf_alloc"

// LeafAlloc leaf_alloc 表映射。
type LeafAlloc struct {
	BizTag      string    `gorm:"column:biz_tag;primaryKey"`
	MaxID       int64     `gorm:"column:max_id"`
	Step        int       `gorm:"column:step"`
	Description *string   `gorm:"column:description"`
	UpdateTime  time.Time `gorm:"column:update_time;autoUpdateTime"`
}

// TableName 表名
func (LeafAlloc) TableName() string { return "leaf_alloc" }

// IDGenerator 号段 ID 生成器。
type IDGenerator interface {
	NextID(ctx context.Context, bizTag string) (int64, error)
	Register(ctx context.Context, bizTag string, initMaxID int64, step int, desc string) error
	Preload(ctx context.Context) error
}

// BizTag 注册一个业务标签及其默认值。
type BizTag struct {
	Name      string
	InitMaxID int64 // 0 → 默认 1_000_000
	Step      int   // 0 → 默认 100_000
	Desc      string
}

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

type buffer struct {
	mu         sync.Mutex
	bizTag     string
	segments   [2]*segment
	current    int
	loadFactor float64
	nextReady  bool
	loading    bool
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
	// gorm FirstOrCreate 不支持 Table 显式指定（会用 struct TableName）；这里
	// 改成 INSERT … ON CONFLICT DO NOTHING（幂等等价），单 SQL 由 DB 原子处理 —
	// 不存在 First 后 Create 的 race window，多 goroutine 同时 Register 同一
	// biz_tag 不会撞 UNIQUE 约束。
	return r.db.WithContext(ctx).Table(leafTable(ctx)).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(a).Error
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
	buffers    sync.Map
	logger     *zap.Logger
}

// New 用 metaDB 构造 IDGenerator，并注册入参里所有 BizTag 到主 + 影子号段表。
//
// 影子表（leaf_alloc_shadow）由独立 init_shadow.sql 创建；如果未创建，
// shadow Register 会失败但**不阻塞主流量** —— 主表 Register 已成功，主流量可用。
func New(metaDB *gorm.DB, logger *zap.Logger, tags []BizTag) (IDGenerator, error) {
	g := &generator{repo: &segmentRepo{db: metaDB}, loadFactor: 0.9, logger: logger}
	ctx := context.Background()
	// 主表 Register
	for _, t := range tags {
		if err := g.Register(ctx, t.Name, t.InitMaxID, t.Step, t.Desc); err != nil {
			return nil, err
		}
	}
	// 影子表 Register（容错：表不存在不阻塞）
	shadowCtx := shadow.WithShadow(ctx, true)
	for _, t := range tags {
		if err := g.Register(shadowCtx, t.Name, t.InitMaxID, t.Step, t.Desc); err != nil {
			logger.Warn("idgen shadow register failed (shadow traffic will fail until leaf_alloc_shadow is provisioned)",
				zap.String("biz_tag", t.Name), zap.Error(err))
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

// Preload 主 + 影子 allocs 都暖一段。
func (g *generator) Preload(ctx context.Context) error {
	for _, isShadow := range []bool{false, true} {
		c := ctx
		if isShadow {
			c = shadow.WithShadow(ctx, true)
		}
		allocs, err := g.repo.listAll(c)
		if err != nil {
			if isShadow {
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
				// 异步预拉下一段：脱离 caller cancel，但保留 ctx 里的 shadow flag。
				asyncCtx := shadow.WithShadow(context.Background(), shadow.IsShadow(ctx))
				go func() { _ = g.loadInto(asyncCtx, biz, buf) }()
			}
			buf.mu.Unlock()
			return id, nil
		}
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

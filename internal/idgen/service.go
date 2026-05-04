package idgen

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"go.uber.org/zap"
)

// IDGenerator 号段模式 ID 生成器。
// 双 Buffer 策略：正常消费 current 号段；当 current 消耗到 loadFactor（默认 90%）时
// 异步预加载 next 号段；current 耗尽后无缝切换到 next。
// ID 严格单调递增，基于 DB max_id 推进，不依赖时钟，重启后时间漂移不影响单调性。
type IDGenerator interface {
	// NextID 从号段 buffer 取下一个 ID（int64，单调递增）
	NextID(ctx context.Context, bizTag string) (int64, error)
	// NextIDStr 返回 string 形式的 NextID
	NextIDStr(ctx context.Context, bizTag string) (string, error)
	// Preload 预热所有已注册 biz_tag 的号段（服务启动时调用）
	Preload(ctx context.Context) error
	// Register 注册新 biz_tag（不存在时插入，存在时幂等跳过）
	Register(ctx context.Context, bizTag string, initMaxID int64, step int, description string) error
}

type segmentIDGenerator struct {
	repo       *segmentRepository
	loadFactor float64
	buffers    sync.Map // map[bizTag]*segmentBuffer
	logger     *zap.Logger
}

// newSegmentIDGenerator 内部构造，由 idgen.go 的 NewIDGeneratorFromManager 调用
func newSegmentIDGenerator(repo *segmentRepository, loadFactor float64, logger *zap.Logger) IDGenerator {
	if loadFactor <= 0 || loadFactor >= 1 {
		loadFactor = 0.9
	}
	return &segmentIDGenerator{
		repo:       repo,
		loadFactor: loadFactor,
		logger:     logger,
	}
}

func (g *segmentIDGenerator) Register(ctx context.Context, bizTag string, initMaxID int64, step int, description string) error {
	if bizTag == "" {
		return fmt.Errorf("idgen: biz_tag must not be empty")
	}
	if step <= 0 {
		step = 10000
	}
	if initMaxID <= 0 {
		initMaxID = 1
	}
	alloc := &LeafAlloc{
		BizTag: bizTag,
		MaxID:  initMaxID,
		Step:   step,
	}
	if description != "" {
		alloc.Description = &description
	}
	return g.repo.register(ctx, alloc)
}

func (g *segmentIDGenerator) Preload(ctx context.Context) error {
	allocs, err := g.repo.getAllBizTags(ctx)
	if err != nil {
		return fmt.Errorf("idgen: preload: %w", err)
	}
	for _, alloc := range allocs {
		if _, loaded := g.buffers.Load(alloc.BizTag); !loaded {
			buf := newSegmentBuffer(alloc.BizTag, g.loadFactor)
			g.buffers.Store(alloc.BizTag, buf)
		}
		buf := g.bufferOf(alloc.BizTag)
		if err := g.loadSegment(ctx, alloc.BizTag, buf); err != nil {
			g.logger.Warn("idgen: preload failed for biz_tag",
				zap.String("bizTag", alloc.BizTag),
				zap.Error(err),
			)
		}
	}
	g.logger.Info("idgen: preload completed", zap.Int("bizTagCount", len(allocs)))
	return nil
}

func (g *segmentIDGenerator) NextID(ctx context.Context, bizTag string) (int64, error) {
	buf, err := g.ensureBuffer(ctx, bizTag)
	if err != nil {
		return 0, err
	}

	for {
		buf.lock()

		seg := buf.currentSeg()
		id := seg.next()
		if id >= 0 {
			if buf.shouldPreload() {
				buf.markLoading()
				go g.asyncLoadNext(context.Background(), bizTag, buf)
			}
			buf.unlock()
			return id, nil
		}

		// current 耗尽，切换到 next
		if buf.nextReady_() {
			buf.switchBuffer()
			buf.unlock()
			continue
		}

		// next 未就绪，同步加载（兜底）
		if !buf.isLoading() {
			buf.markLoading()
			buf.unlock()
			if err := g.loadSegment(ctx, bizTag, buf); err != nil {
				return 0, fmt.Errorf("idgen: NextID(%s): reload failed: %w", bizTag, err)
			}
			continue
		}

		// 正在异步加载，让出时间片等待（避免忙转消耗 CPU）
		buf.unlock()
		runtime.Gosched()
	}
}

func (g *segmentIDGenerator) NextIDStr(ctx context.Context, bizTag string) (string, error) {
	id, err := g.NextID(ctx, bizTag)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d", id), nil
}

// ─── 内部辅助 ──────────────────────────────────────────────────────────────────

func (g *segmentIDGenerator) bufferOf(bizTag string) *segmentBuffer {
	v, _ := g.buffers.Load(bizTag)
	return v.(*segmentBuffer)
}

func (g *segmentIDGenerator) ensureBuffer(ctx context.Context, bizTag string) (*segmentBuffer, error) {
	v, loaded := g.buffers.Load(bizTag)
	if loaded {
		return v.(*segmentBuffer), nil
	}

	alloc, err := g.repo.getByBizTag(ctx, bizTag)
	if err != nil {
		return nil, err
	}
	if alloc == nil {
		return nil, fmt.Errorf("idgen: biz_tag not found: %s", bizTag)
	}

	buf := newSegmentBuffer(bizTag, g.loadFactor)
	actual, _ := g.buffers.LoadOrStore(bizTag, buf)
	buf = actual.(*segmentBuffer)

	buf.lock()
	if buf.currentSeg() == nil {
		buf.unlock()
		if err := g.loadSegment(ctx, bizTag, buf); err != nil {
			return nil, err
		}
	} else {
		buf.unlock()
	}
	return buf, nil
}

func (g *segmentIDGenerator) loadSegment(ctx context.Context, bizTag string, buf *segmentBuffer) error {
	alloc, err := g.repo.allocNextSegment(ctx, bizTag)
	if err != nil {
		return err
	}
	start := alloc.MaxID - int64(alloc.Step)
	seg := newSegment(start, alloc.Step)

	buf.lock()
	defer buf.unlock()

	if buf.currentSeg() == nil {
		buf.initialize(seg)
	} else {
		buf.setNextSegment(seg)
	}

	g.logger.Debug("idgen: segment loaded",
		zap.String("bizTag", bizTag),
		zap.Int64("start", start),
		zap.Int64("end", alloc.MaxID),
		zap.Int("step", alloc.Step),
	)
	return nil
}

func (g *segmentIDGenerator) asyncLoadNext(ctx context.Context, bizTag string, buf *segmentBuffer) {
	if err := g.loadSegment(ctx, bizTag, buf); err != nil {
		g.logger.Warn("idgen: async load segment failed",
			zap.String("bizTag", bizTag),
			zap.Error(err),
		)
		buf.lock()
		buf.cancelLoading()
		buf.unlock()
	}
}

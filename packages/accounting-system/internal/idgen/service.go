package idgen

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/xiongwp/payment-util/shadow"
	"go.uber.org/zap"
)

// effectiveBufferKey 把 (bizTag, isShadow) 折成单一 buffer key；主流量号段
// 和影子号段独立维护，互不感知（leaf_alloc / leaf_alloc_shadow 双 buffer）。
func effectiveBufferKey(bizTag string, isShadow bool) string {
	if isShadow {
		return bizTag + "::shadow"
	}
	return bizTag
}

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
	lastLoad   sync.Map // map[bizTag]time.Time —— 自适应 step 用，跟踪上次 reload 时间
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

// Register 注册 biz_tag。同一 bizTag 同时在 leaf_alloc 主表和 leaf_alloc_shadow
// 影子表都注册一份，让主和影子号段互不感知地各自递增。
//
// 影子表（leaf_alloc_shadow）由独立 init_shadow.sql 创建；如果未创建，shadow
// Register 会失败但**不阻塞主流量** —— 主表 Register 已成功，主流量可用。
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
	// 主表 register
	if err := g.repo.register(ctx, alloc); err != nil {
		return err
	}
	// 影子表 register（容错：表不存在不阻塞主流量）
	shadowCtx := shadow.WithShadow(ctx, true)
	shadowAlloc := *alloc // copy
	if err := g.repo.register(shadowCtx, &shadowAlloc); err != nil {
		g.logger.Warn("idgen: shadow register failed (shadow traffic will fail until leaf_alloc_shadow is provisioned)",
			zap.String("bizTag", bizTag),
			zap.Error(err),
		)
	}
	return nil
}

// Preload 主 + 影子 allocs 都暖一段。
func (g *segmentIDGenerator) Preload(ctx context.Context) error {
	for _, isShadow := range []bool{false, true} {
		c := ctx
		if isShadow {
			c = shadow.WithShadow(ctx, true)
		}
		allocs, err := g.repo.getAllBizTags(c)
		if err != nil {
			if isShadow {
				g.logger.Warn("idgen: shadow preload failed (skipping)", zap.Error(err))
				continue
			}
			return fmt.Errorf("idgen: preload: %w", err)
		}
		for _, alloc := range allocs {
			key := effectiveBufferKey(alloc.BizTag, isShadow)
			if _, loaded := g.buffers.Load(key); !loaded {
				buf := newSegmentBuffer(alloc.BizTag, g.loadFactor)
				g.buffers.Store(key, buf)
			}
			buf := g.bufferOfKey(key)
			if err := g.loadSegment(c, alloc.BizTag, buf); err != nil {
				g.logger.Warn("idgen: preload failed for biz_tag",
					zap.String("bizTag", alloc.BizTag),
					zap.Bool("shadow", isShadow),
					zap.Error(err),
				)
			}
		}
		g.logger.Info("idgen: preload completed",
			zap.Bool("shadow", isShadow), zap.Int("bizTagCount", len(allocs)))
	}
	return nil
}

func (g *segmentIDGenerator) NextID(ctx context.Context, bizTag string) (int64, error) {
	buf, err := g.ensureBuffer(ctx, bizTag)
	if err != nil {
		return 0, err
	}
	isShadow := shadow.IsShadow(ctx)

	for {
		buf.lock()

		seg := buf.currentSeg()
		id := seg.next()
		if id >= 0 {
			if buf.shouldPreload() {
				buf.markLoading()
				// 异步预拉脱离 caller cancel，但保留 shadow flag
				asyncCtx := shadow.WithShadow(context.Background(), isShadow)
				go g.asyncLoadNext(asyncCtx, bizTag, buf)
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

// bufferOfKey 直接按 effective key 取 buffer（Preload 用）。
func (g *segmentIDGenerator) bufferOfKey(key string) *segmentBuffer {
	v, _ := g.buffers.Load(key)
	return v.(*segmentBuffer)
}

func (g *segmentIDGenerator) ensureBuffer(ctx context.Context, bizTag string) (*segmentBuffer, error) {
	isShadow := shadow.IsShadow(ctx)
	key := effectiveBufferKey(bizTag, isShadow)
	v, loaded := g.buffers.Load(key)
	if loaded {
		return v.(*segmentBuffer), nil
	}

	alloc, err := g.repo.getByBizTag(ctx, bizTag)
	if err != nil {
		return nil, err
	}
	if alloc == nil {
		return nil, fmt.Errorf("idgen: biz_tag not found: %s (shadow=%v)", bizTag, isShadow)
	}

	buf := newSegmentBuffer(bizTag, g.loadFactor)
	actual, _ := g.buffers.LoadOrStore(key, buf)
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

	// 自适应步长（P1-12）：上次 reload 与本次间隔很短 → step 倍增。
	// 这样高 QPS bizTag 自动从 100k 涨到 1.6M（max 16×），低 QPS 保持 100k。
	// 步长写回 leaf_alloc.step 后下次 allocNextSegment 自然取新值。
	now := time.Now()
	g.adaptStepLocked(ctx, bizTag, alloc, now)

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

// adaptStepLocked 自适应调整 step。仅在间隔过短时倍增；不持有 buf.mu，所以可以
// 跟 buf 操作并行。会话级状态用 g.lastLoad（sync.Map），多副本场景互不影响：
// 各副本自己观测自己的 reload 节奏，写回的 step 是全局共享（leaf_alloc 同一行），
// 副本间事实上互相协作（任一副本检测到高 QPS 就调高，所有副本受益）。
func (g *segmentIDGenerator) adaptStepLocked(ctx context.Context, bizTag string, alloc *LeafAlloc, now time.Time) {
	prevAny, ok := g.lastLoad.Load(bizTag)
	g.lastLoad.Store(bizTag, now)
	if !ok {
		return
	}
	prev, _ := prevAny.(time.Time)
	if now.Sub(prev) >= adaptiveStepFastThreshold {
		return
	}
	maxStep := defaultStep * adaptiveStepMaxMultiplier
	if alloc.Step >= maxStep {
		return
	}
	newStep := alloc.Step * 2
	if newStep > maxStep {
		newStep = maxStep
	}
	if err := g.repo.updateStep(ctx, bizTag, newStep); err != nil {
		g.logger.Warn("idgen: adaptive step update failed",
			zap.String("bizTag", bizTag), zap.Int("from", alloc.Step), zap.Int("to", newStep),
			zap.Error(err))
		return
	}
	g.logger.Info("idgen: adaptive step bumped (high QPS detected)",
		zap.String("bizTag", bizTag),
		zap.Int("from", alloc.Step),
		zap.Int("to", newStep),
		zap.Duration("interval", now.Sub(prev)))
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

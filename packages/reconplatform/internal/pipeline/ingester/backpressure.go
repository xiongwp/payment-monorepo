// backpressure.go — REL-2: Redis 压力大时暂停 Kafka 消费.
//
// 问题:
//   ingester 全速消费时, candidate.Layer.Put → Redis 写入压力上升 → Redis
//   used_memory 或 trigger 队列堆积超阈值 → matcher 处理慢于生产 → Redis OOM 或
//   queue 雪崩. 此时硬撑只会让事态更糟.
//
// 方案:
//   - Controller 每 5s 检查 Redis 信号:
//     a) trigger queue depth > HighWaterDepth
//     b) Redis used_memory / max_memory > HighWaterMem (default 80%)
//   - 任一触发 → kgo.PauseFetchTopics(cfg.Topics...) 暂停消费.
//   - 都降到 LowWater 以下 → ResumeFetchTopics 恢复.
//   - 暂停期间 ingester 主循环空转 PollRecords (返回空), CPU 几乎 0.
//
// 收益:
//   - 防 Redis OOM: 上游让步, matcher 有时间消化.
//   - 防 Kafka offset 风暴: 暂停后不消费, lag 涨但 Kafka 自身没事.
//   - 自愈: matcher 处理完降到 LowWater 即自动 resume, 无须人工.
//
// 旁路:
//   - 没挂 Layer Stats → 退化为只看 Redis memory (仍可用).
//   - 没挂 Redis → 完全 disabled (不暂停).

package ingester

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.uber.org/zap"

	"reconcile-system/internal/pipeline/candidate"
)

// BackpressureConfig 配置阈值.
type BackpressureConfig struct {
	// Trigger queue depth 阈值 (LIST 长度).
	HighWaterDepth int64 // 默认 50000
	LowWaterDepth  int64 // 默认 10000

	// Redis used_memory / maxmemory 比例阈值 (0..1).
	// 没设 maxmemory 时这条不生效 (always 0).
	HighWaterMemFrac float64 // 默认 0.80
	LowWaterMemFrac  float64 // 默认 0.60

	// 检查间隔.
	Interval time.Duration // 默认 5s
}

// DefaultBackpressureConfig 默认.
func DefaultBackpressureConfig() BackpressureConfig {
	return BackpressureConfig{
		HighWaterDepth:   50000,
		LowWaterDepth:    10000,
		HighWaterMemFrac: 0.80,
		LowWaterMemFrac:  0.60,
		Interval:         5 * time.Second,
	}
}

// BackpressureController 周期检查信号并 pause/resume kgo 客户端.
type BackpressureController struct {
	cfg    BackpressureConfig
	cl     *kgo.Client
	rdb    redis.UniversalClient
	layer  candidate.Layer
	topics []string
	logger *zap.Logger

	paused atomic.Bool // 当前状态
	transitions atomic.Int64
	currentDepth atomic.Int64
	currentMemFrac atomic.Uint64 // *1000 存,取出再除
}

// NewBackpressureController 构造.
func NewBackpressureController(
	cfg BackpressureConfig,
	cl *kgo.Client,
	rdb redis.UniversalClient,
	layer candidate.Layer,
	topics []string,
	logger *zap.Logger,
) *BackpressureController {
	if cfg.HighWaterDepth <= 0 {
		cfg.HighWaterDepth = 50000
	}
	if cfg.LowWaterDepth <= 0 {
		cfg.LowWaterDepth = 10000
	}
	if cfg.HighWaterMemFrac <= 0 {
		cfg.HighWaterMemFrac = 0.80
	}
	if cfg.LowWaterMemFrac <= 0 {
		cfg.LowWaterMemFrac = 0.60
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BackpressureController{
		cfg: cfg, cl: cl, rdb: rdb, layer: layer, topics: topics, logger: logger,
	}
}

// Run 阻塞循环, 退出由 ctx 控制.
func (b *BackpressureController) Run(ctx context.Context) {
	t := time.NewTicker(b.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// 退出前主动 resume,避免下次启动 paused 状态.
			if b.paused.Load() && b.cl != nil {
				b.cl.ResumeFetchTopics(b.topics...)
			}
			return
		case <-t.C:
			b.check(ctx)
		}
	}
}

// check 单次检查.
func (b *BackpressureController) check(ctx context.Context) {
	depth := b.readDepth(ctx)
	memFrac := b.readMemFrac(ctx)
	b.currentDepth.Store(depth)
	b.currentMemFrac.Store(uint64(memFrac * 1000))

	currentlyPaused := b.paused.Load()

	highTrigger := depth >= b.cfg.HighWaterDepth ||
		(memFrac > 0 && memFrac >= b.cfg.HighWaterMemFrac)
	lowTrigger := depth <= b.cfg.LowWaterDepth &&
		(memFrac == 0 || memFrac <= b.cfg.LowWaterMemFrac)

	switch {
	case !currentlyPaused && highTrigger:
		if b.cl != nil {
			b.cl.PauseFetchTopics(b.topics...)
		}
		b.paused.Store(true)
		b.transitions.Add(1)
		b.logger.Warn("backpressure: paused",
			zap.Int64("depth", depth),
			zap.Float64("mem_frac", memFrac),
			zap.Strings("topics", b.topics))
	case currentlyPaused && lowTrigger:
		if b.cl != nil {
			b.cl.ResumeFetchTopics(b.topics...)
		}
		b.paused.Store(false)
		b.transitions.Add(1)
		b.logger.Info("backpressure: resumed",
			zap.Int64("depth", depth),
			zap.Float64("mem_frac", memFrac))
	}
}

// readDepth 读 trigger queue depth (跨 shard 合并).
func (b *BackpressureController) readDepth(ctx context.Context) int64 {
	if b.layer == nil {
		return 0
	}
	stats, err := b.layer.Stats(ctx)
	if err != nil {
		return 0
	}
	return int64(stats["trigger_queue_depth"])
}

// readMemFrac Redis used_memory / maxmemory.
//
// INFO memory 返回多行 KV. 没设 maxmemory (=0) → 返 0.0 不参与判定.
func (b *BackpressureController) readMemFrac(ctx context.Context) float64 {
	if b.rdb == nil {
		return 0
	}
	info, err := b.rdb.Info(ctx, "memory").Result()
	if err != nil {
		return 0
	}
	var used, maxmem int64
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "used_memory:"); ok {
			used, _ = strconv.ParseInt(v, 10, 64)
		}
		if v, ok := strings.CutPrefix(line, "maxmemory:"); ok {
			maxmem, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if maxmem <= 0 {
		return 0
	}
	return float64(used) / float64(maxmem)
}

// BackpressureStats 给监控 / 调试看.
type BackpressureStats struct {
	Paused       bool    `json:"paused"`
	Transitions  int64   `json:"transitions"`
	CurrentDepth int64   `json:"current_depth"`
	CurrentMemFrac float64 `json:"current_mem_frac"`
}

// Stats 当前状态快照.
func (b *BackpressureController) Stats() BackpressureStats {
	return BackpressureStats{
		Paused:        b.paused.Load(),
		Transitions:   b.transitions.Load(),
		CurrentDepth:  b.currentDepth.Load(),
		CurrentMemFrac: float64(b.currentMemFrac.Load()) / 1000,
	}
}

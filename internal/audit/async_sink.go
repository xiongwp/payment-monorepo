// async_sink.go: 异步批量写入包装器。
//
// 问题：audit Sink 在 hot path 上同步调（service.Screen 每笔决策必写）。
// 包装一层 inner Sink (LogSink / ClickHouseSink / KafkaSink / FileSink) 时，
// inner 的 latency 直接打到 Screen p99：
//   - LogSink (本地 stdout)：~1µs，可忽略
//   - ClickHouseSink (远程 INSERT)：~5-50ms，会拖慢 Screen
//   - KafkaSink (网络 produce)：~1-5ms 平均，p99 可能更高
//
// AsyncBatchSink 解耦：Write 把 record 塞 channel buffer 立即返回；后台
// goroutine 按 batch_size / flush_interval 触发 inner 批量写。
//
//   - 默认 batch_size=1000 + flush_interval=1s → 大致跟 ClickHouse Kafka
//     engine 吃单条 INSERT 的甜蜜点对齐
//   - buffer 满 → drop_oldest（fail-open；audit 不能阻塞 Screen 主路径）
//     metric `risk_audit_async_dropped_total` 计数让 SRE 看见
//   - graceful shutdown：flush 余下 record 再退出（生产关键）
//
// inner.Write 里 panic 会被 catch + log，不影响后续 batch（防一条 record
// 把整个 sink 拖死）。
package audit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// AsyncBatchSink 把 Write 排队到 channel；后台 goroutine 批量推 inner。
type AsyncBatchSink struct {
	inner    Sink
	logger   *zap.Logger
	queue    chan *DecisionAudit
	batch    int
	flush    time.Duration
	dropped  atomic.Uint64
	wg       sync.WaitGroup
	stopOnce sync.Once
	stop     chan struct{}
}

// NewAsyncBatchSink 起一个后台 worker。
//
//	queueSize: channel buffer；满了时 Write 走 drop（不阻塞）
//	batchSize: 单批最大条数
//	flush:     强制 flush 间隔（即使 batch 没满）
//
// queueSize=0 → 8192；batchSize=0 → 1000；flush=0 → 1s。
func NewAsyncBatchSink(inner Sink, queueSize, batchSize int, flush time.Duration, logger *zap.Logger) *AsyncBatchSink {
	if queueSize <= 0 {
		queueSize = 8192
	}
	if batchSize <= 0 {
		batchSize = 1000
	}
	if flush <= 0 {
		flush = 1 * time.Second
	}
	s := &AsyncBatchSink{
		inner:  inner,
		logger: logger,
		queue:  make(chan *DecisionAudit, queueSize),
		batch:  batchSize,
		flush:  flush,
		stop:   make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// Write 入队；buffer 满 → drop_oldest（atomic 计数）。永不阻塞主路径。
func (s *AsyncBatchSink) Write(_ context.Context, a *DecisionAudit) {
	if s == nil || a == nil {
		return
	}
	select {
	case s.queue <- a:
	default:
		// buffer 满；尝试 drop oldest 给新的腾位置（fail-open）
		select {
		case <-s.queue:
			s.dropped.Add(1)
		default:
		}
		select {
		case s.queue <- a:
		default:
			s.dropped.Add(1)
		}
	}
}

func (s *AsyncBatchSink) run() {
	defer s.wg.Done()
	buf := make([]*DecisionAudit, 0, s.batch)
	timer := time.NewTimer(s.flush)
	defer timer.Stop()
	flushBuf := func() {
		if len(buf) == 0 {
			return
		}
		s.flushBatch(buf)
		buf = buf[:0]
	}
	for {
		select {
		case a := <-s.queue:
			buf = append(buf, a)
			if len(buf) >= s.batch {
				flushBuf()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(s.flush)
			}
		case <-timer.C:
			flushBuf()
			timer.Reset(s.flush)
		case <-s.stop:
			// drain queue 再退出
			for {
				select {
				case a := <-s.queue:
					buf = append(buf, a)
				default:
					flushBuf()
					return
				}
				if len(buf) >= s.batch {
					flushBuf()
				}
			}
		}
	}
}

// flushBatch 调 inner.Write 串行写每条；inner panic 被 catch 不影响后续。
// 如果 inner 实现了 BatchSink 接口直接走批量 API。
func (s *AsyncBatchSink) flushBatch(batch []*DecisionAudit) {
	defer func() {
		if r := recover(); r != nil && s.logger != nil {
			s.logger.Error("audit async batch panic", zap.Any("recover", r))
		}
	}()
	if bs, ok := s.inner.(BatchSink); ok {
		// inner 自己有批量优化（如 ClickHouseSink batch INSERT）
		bs.WriteBatch(context.Background(), batch)
		return
	}
	for _, a := range batch {
		s.inner.Write(context.Background(), a)
	}
}

// BatchSink 可选接口：sink 实现批量写时让 AsyncBatchSink 直接调而不是循环
// 单条调（CH / Kafka producer 批量 produce 比逐条快几个数量级）。
type BatchSink interface {
	WriteBatch(ctx context.Context, batch []*DecisionAudit)
}

// Stop 等 worker 把队列里剩余条目 flush 完再退。生产 main.go 在 shutdown
// hook 里调；避免重启丢未落库的 audit。
func (s *AsyncBatchSink) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Dropped 累计被丢的 audit 数。给 metric "risk_audit_async_dropped_total" 用。
// > 0 = inner sink 跟不上 / queue 太小；调 queueSize / batchSize。
func (s *AsyncBatchSink) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// QueueLen 当前队列深度（非精确：channel cap 减读端）。给 metric / 健康
// 检查用。
func (s *AsyncBatchSink) QueueLen() int {
	if s == nil {
		return 0
	}
	return len(s.queue)
}

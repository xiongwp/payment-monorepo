// Package reshard — V1 → V2 数据迁移框架（reshard SOP Phase 3）。
//
// 适用：accounting-system / order-core / card-center / 任何分片表升级到 V2 layout。
//
// 整体设计：
//
//	[V1 source table] -- ScanBatch -→ [migrator pipeline]
//	                                       |
//	                                       v
//	                                  TransformRow
//	                                       |
//	                                       v
//	                                  WriteV2(dest)
//	                                       |
//	                                       v
//	                                 Checkpoint(progress)
//
// 关键能力：
//
//   - **断点续传**：每批 commit 时持久化 (sourceShard, lastPK)，重启从上次位置继续
//   - **速率限制**：rate.Limiter 控制每秒处理行数，防迁移把生产 DB 打爆
//   - **暂停 / 恢复**：admin 可以 dynamically Pause()/Resume()，紧急时 Stop()
//   - **多源 shard 并发**：每个 V1 shard 一个 worker，互相独立（V2 dest 写入时 PK
//     不撞，因为 V2 layout 用不同 globalTblIdx）
//   - **幂等**：dest 表用 INSERT IGNORE / ON CONFLICT，重跑同一批不会脏写
//   - **diff 监控**：可选 verifier，定期抽样比对 V1 vs V2 行内容，差异 > 阈值告警
//
// **使用方提供**:
//
//	tableSpec.ScanV1(ctx, shard, lastPK, limit) ([]Row, lastPKReturned, error)
//	tableSpec.TransformV1ToV2(row) (Row, dstShardKey, error)
//	tableSpec.WriteV2(ctx, dstShardKey, rows []Row) error
//	checkpointStore.Save(ctx, shard, lastPK) / Load(ctx, shard) / List(ctx)
//
// **不做的事**:
//
//   - 不锁源表 / 不开事务跨 V1+V2（业务侧 dual-write 已经保证 in-flight 写一致）
//   - 不做反向回滚（rollback 是改 CurrentLayoutVersion 回 V1，不删 V2 表数据）
package reshard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/time/rate"
)

// Row 一条业务数据；具体形状由 TableSpec.ScanV1 决定（可以是 map / struct）。
type Row = map[string]any

// TableSpec 业务表迁移规格。
//
// 每个待迁移的表（如 voucher / account_balance / payment_intent）都实现一份。
type TableSpec interface {
	// Name 给日志 / metrics 的人类可读名（如 "accounting.voucher"）。
	Name() string

	// V1Shards 列出所有 V1 源 shard（[(dbIdx, tblIdx), ...]）。
	// 典型：100 项（10×10 V1 layout）。worker 会按这个列表起 N 个 goroutine。
	V1Shards() [][2]int

	// ScanV1 从 V1 shard 读一批行。
	//
	//	limit       本批最大行数（典型 1000）
	//	lastPK      上次返回的最后一条 PK；首次调 0
	//	返回 rows   本批数据
	//	返回 maxPK  本批最大 PK，下次作 lastPK 传入
	//	返回 done   true 表示该 shard 没有更多数据
	ScanV1(ctx context.Context, shard [2]int, lastPK int64, limit int) (rows []Row, maxPK int64, done bool, err error)

	// TransformV1ToV2 转换单行：重新计算 V2 路由（dstDB, dstTbl）+ 调整字段
	// （如果 V2 layout 改了 PK 编码）。
	//
	// 返回的 Row 可以原样（V1/V2 schema 不变时）或新结构。
	TransformV1ToV2(ctx context.Context, row Row) (out Row, dstDB, dstTbl int, err error)

	// WriteV2 把一批已转换的行写到目标 V2 shard。
	//
	// **必须幂等**：用 INSERT IGNORE / ON CONFLICT DO NOTHING，
	// 重跑同一批不会脏写也不会报 duplicate key error。
	WriteV2(ctx context.Context, dstDB, dstTbl int, rows []Row) error

	// PrimaryKey 取行的 PK 数值（用于 checkpoint）。
	PrimaryKey(row Row) int64
}

// CheckpointStore 持久化每 shard 的进度。
//
// 实现：通常用 MySQL 单表 `reshard_checkpoint(table_name, db_idx, tbl_idx, last_pk, updated_at)`。
type CheckpointStore interface {
	Save(ctx context.Context, tableName string, shard [2]int, lastPK int64, total int64) error
	Load(ctx context.Context, tableName string, shard [2]int) (lastPK int64, total int64, err error)
}

// Config 迁移配置。
type Config struct {
	BatchSize    int           // 每批行数，默认 1000
	RatePerSec   float64       // 全局速率上限（rows/sec），默认 5000，<=0 不限速
	Concurrency  int           // 并发 shard worker 数，默认 8
	IdleSleep    time.Duration // shard 跑完后到下次轮询的空窗，默认 30s
	CommitEvery  int           // 每 N 批 commit 一次 checkpoint，默认 10
	StuckTimeout time.Duration // 单批写入超时，默认 30s
}

// DefaultConfig 给定保守默认值。
func DefaultConfig() Config {
	return Config{
		BatchSize:    1000,
		RatePerSec:   5000,
		Concurrency:  8,
		IdleSleep:    30 * time.Second,
		CommitEvery:  10,
		StuckTimeout: 30 * time.Second,
	}
}

// Stats 当前迁移状态快照（给 admin / Prometheus 用）。
type Stats struct {
	TotalRows    int64                   // 累计已写 V2 行数
	BatchesOK    int64                   // 累计成功批次
	BatchesFail  int64                   // 累计失败批次
	ShardDone    int64                   // 已完成的 shard 数
	ShardTotal   int                     // 总 shard 数
	StartedAt    time.Time
	LastEventAt  time.Time
	PerShardTail map[[2]int]int64        // 每 shard 当前 lastPK
}

// Migrator V1 → V2 迁移器。
type Migrator struct {
	spec  TableSpec
	cp    CheckpointStore
	cfg   Config
	limit *rate.Limiter
	log   *zap.Logger

	// runtime
	mu          sync.Mutex
	paused      atomic.Bool
	stopCh      chan struct{}
	wg          sync.WaitGroup
	totalRows   atomic.Int64
	batchesOK   atomic.Int64
	batchesFail atomic.Int64
	shardDone   atomic.Int64
	startedAt   atomic.Int64 // unix ns
	lastEventAt atomic.Int64
	perShard    sync.Map // [2]int → *atomic.Int64
}

// New 构造。
func New(spec TableSpec, cp CheckpointStore, cfg Config, log *zap.Logger) *Migrator {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1000
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = 30 * time.Second
	}
	if cfg.CommitEvery <= 0 {
		cfg.CommitEvery = 10
	}
	if cfg.StuckTimeout <= 0 {
		cfg.StuckTimeout = 30 * time.Second
	}
	var lim *rate.Limiter
	if cfg.RatePerSec > 0 {
		lim = rate.NewLimiter(rate.Limit(cfg.RatePerSec), cfg.BatchSize) // burst = 1 batch
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Migrator{
		spec:   spec,
		cp:     cp,
		cfg:    cfg,
		limit:  lim,
		log:    log,
		stopCh: make(chan struct{}),
	}
}

// Start 启动并发 shard worker。非阻塞；caller 在 fx.OnStop 调 Stop。
//
// 多次 Start 是 noop（用 startedAt 判定）。
func (m *Migrator) Start(ctx context.Context) {
	if !m.startedAt.CompareAndSwap(0, time.Now().UnixNano()) {
		return // 已在运行
	}
	shards := m.spec.V1Shards()
	m.log.Info("reshard migrator starting",
		zap.String("table", m.spec.Name()),
		zap.Int("shards", len(shards)),
		zap.Int("concurrency", m.cfg.Concurrency))

	// 限制并发：Concurrency 个 worker 共抢 shards。简单 channel 派发。
	jobs := make(chan [2]int, len(shards))
	for _, s := range shards {
		jobs <- s
	}
	close(jobs)

	for i := 0; i < m.cfg.Concurrency; i++ {
		m.wg.Add(1)
		go m.worker(ctx, jobs)
	}
}

// Pause 暂停所有 worker（不杀）；Resume 后从上次 lastPK 继续。
// 适用紧急控流（DB 抖动 / 误配速率）；不影响已 commit 的 checkpoint。
func (m *Migrator) Pause()  { m.paused.Store(true) }
func (m *Migrator) Resume() { m.paused.Store(false) }

// Stop 优雅停止：通知 worker 退出 + Wait。Stop 后不可 Restart。
func (m *Migrator) Stop() {
	select {
	case <-m.stopCh:
		// 已 Stop
	default:
		close(m.stopCh)
	}
	m.wg.Wait()
}

// IsRunning 是否还在跑（startedAt > 0 且 stopCh 未 close）。
func (m *Migrator) IsRunning() bool {
	if m.startedAt.Load() == 0 {
		return false
	}
	select {
	case <-m.stopCh:
		return false
	default:
		return true
	}
}

// SnapshotStats 当前状态快照。
func (m *Migrator) SnapshotStats() Stats {
	per := make(map[[2]int]int64)
	m.perShard.Range(func(k, v any) bool {
		shard := k.([2]int)
		ptr := v.(*atomic.Int64)
		per[shard] = ptr.Load()
		return true
	})
	return Stats{
		TotalRows:    m.totalRows.Load(),
		BatchesOK:    m.batchesOK.Load(),
		BatchesFail:  m.batchesFail.Load(),
		ShardDone:    m.shardDone.Load(),
		ShardTotal:   len(m.spec.V1Shards()),
		StartedAt:    time.Unix(0, m.startedAt.Load()),
		LastEventAt:  time.Unix(0, m.lastEventAt.Load()),
		PerShardTail: per,
	}
}

func (m *Migrator) worker(ctx context.Context, jobs <-chan [2]int) {
	defer m.wg.Done()
	for shard := range jobs {
		if err := ctx.Err(); err != nil {
			return
		}
		select {
		case <-m.stopCh:
			return
		default:
		}
		m.runShard(ctx, shard)
	}
}

// runShard 跑单个 V1 shard 直到 done 或被 Stop。
//
// 流程：
//
//	checkpoint.Load(shard) → lastPK
//	loop:
//	  paused? 等
//	  ScanV1(shard, lastPK, BatchSize) → rows, maxPK, done
//	  for row in rows: Transform → 按 (dstDB, dstTbl) 分桶
//	  buckets.foreach: WriteV2(dstDB, dstTbl, batch)
//	  rate.Wait(batch)
//	  每 CommitEvery 批 → checkpoint.Save(shard, maxPK)
//	  done? break
//	  没行? sleep IdleSleep
func (m *Migrator) runShard(ctx context.Context, shard [2]int) {
	tail, _, err := m.cp.Load(ctx, m.spec.Name(), shard)
	if err != nil {
		m.log.Warn("checkpoint load failed (start from 0)",
			zap.Any("shard", shard), zap.Error(err))
	}
	tracker, _ := m.perShard.LoadOrStore(shard, &atomic.Int64{})
	tailPtr := tracker.(*atomic.Int64)
	tailPtr.Store(tail)

	batches := 0
	for {
		// 控制信号
		if err := ctx.Err(); err != nil {
			return
		}
		select {
		case <-m.stopCh:
			return
		default:
		}
		if m.paused.Load() {
			// 暂停期不退出，每秒醒一次看看
			select {
			case <-time.After(time.Second):
				continue
			case <-m.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}

		// 拉一批
		rows, maxPK, done, err := m.spec.ScanV1(ctx, shard, tail, m.cfg.BatchSize)
		if err != nil {
			m.batchesFail.Add(1)
			m.log.Warn("scan v1 failed",
				zap.Any("shard", shard), zap.Int64("lastPK", tail), zap.Error(err))
			// scan 失败不等于数据问题；睡一段再试
			select {
			case <-time.After(m.cfg.IdleSleep):
			case <-m.stopCh:
				return
			case <-ctx.Done():
				return
			}
			continue
		}

		if len(rows) > 0 {
			if err := m.processBatch(ctx, rows); err != nil {
				m.batchesFail.Add(1)
				m.log.Warn("process batch failed",
					zap.Any("shard", shard), zap.Error(err))
				select {
				case <-time.After(m.cfg.IdleSleep):
				case <-m.stopCh:
					return
				case <-ctx.Done():
					return
				}
				continue
			}
			m.batchesOK.Add(1)
			m.totalRows.Add(int64(len(rows)))
			tail = maxPK
			tailPtr.Store(tail)
			m.lastEventAt.Store(time.Now().UnixNano())
			batches++

			// 周期性持久化进度
			if batches%m.cfg.CommitEvery == 0 {
				if err := m.cp.Save(ctx, m.spec.Name(), shard, tail, m.totalRows.Load()); err != nil {
					m.log.Warn("checkpoint save failed",
						zap.Any("shard", shard), zap.Error(err))
				}
			}
		}

		if done {
			// 收尾：最后一次 commit checkpoint
			_ = m.cp.Save(ctx, m.spec.Name(), shard, tail, m.totalRows.Load())
			m.shardDone.Add(1)
			m.log.Info("shard migration done",
				zap.Any("shard", shard),
				zap.Int64("last_pk", tail))
			return
		}

		// 没行 → 空轮询休眠（防 CPU 空转）
		if len(rows) == 0 {
			select {
			case <-time.After(m.cfg.IdleSleep):
			case <-m.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

// processBatch 对一批 V1 行做 Transform → 按 dst shard 分桶 → 批量写 V2。
//
// 速率限制：在 WriteV2 之前 rate.WaitN(batch_size)，让全局每秒写入不超过 RatePerSec。
func (m *Migrator) processBatch(ctx context.Context, rows []Row) error {
	if m.limit != nil {
		// burst = batchSize；WaitN 必要时阻塞但不超过 ctx.Done。
		if err := m.limit.WaitN(ctx, len(rows)); err != nil {
			return err
		}
	}

	// 按 (dstDB, dstTbl) 分桶
	type key = [2]int
	buckets := make(map[key][]Row, 4)
	for _, row := range rows {
		out, db, tbl, err := m.spec.TransformV1ToV2(ctx, row)
		if err != nil {
			// 单条失败不阻断整批；记录后跳过（生产会写 dead-letter table）
			m.log.Warn("transform failed (skipping row)",
				zap.Any("pk", m.spec.PrimaryKey(row)), zap.Error(err))
			continue
		}
		k := key{db, tbl}
		buckets[k] = append(buckets[k], out)
	}

	// 写各 dst shard。任一 dst 写失败整批失败（caller 重试）。
	wctx, cancel := context.WithTimeout(ctx, m.cfg.StuckTimeout)
	defer cancel()
	for k, b := range buckets {
		if err := m.spec.WriteV2(wctx, k[0], k[1], b); err != nil {
			return err
		}
	}
	return nil
}

// ErrAlreadyStopped Stop() 后再次操作返此错误。
var ErrAlreadyStopped = errors.New("reshard: migrator already stopped")

// reversal_retry_worker.go — SP-AC-7 R5: 消费 reversal_retry_outbox 自动重试.
//
// 跟 refund.go 协作:
//   - refund.go 失败时 (engine.ReversalApply.Apply 返 err) → Enqueue 一条 outbox row
//   - 本 worker 周期扫 pending 行, 调 retry func 再试; 成功 → MarkDone; 失败 → 留待下次
//
// 工作流幂等保证: ApplyReversalAtomic 用 reversal.ID UNIQUE + ON DUPLICATE KEY 兜底,
// 多次调用 0 重复落账. Transfer.AddReversedAmount 的 +delta 也通过 outbox row 唯一 (reversal_id)
// 防止累加.
//
// 注意: 真生产部署时 worker 必须挂 CronLease (workflow/cron_lease.go) 防多副本并发
// 处理同一 reversal_id.
package workflow

import (
	"context"
	"errors"
	"time"

	"github.com/xiongwp/split-payment/internal/domain"

	"go.uber.org/zap"
)

// ReversalOutboxClaimer 给 worker 用的最小接口 (避免直接 import repo, 方便单测).
type ReversalOutboxClaimer interface {
	Claim(ctx context.Context, limit int) ([]ReversalOutboxJob, error)
	MarkDone(ctx context.Context, id int64) error
	MarkDeadLetter(ctx context.Context, id int64, lastErr string) error
	UpdateError(ctx context.Context, id int64, lastErr string) error
}

// ReversalOutboxJob 跟 repo.ReversalOutboxRow 同形态, 但 workflow 自己定义避免循环 import.
type ReversalOutboxJob struct {
	ID         int64
	ReversalID string
	TransferID string
	DeltaMinor int64
	RetryCount int
	MaxRetry   int
}

// ReversalRetryWorkerConfig.
type ReversalRetryWorkerConfig struct {
	Interval  time.Duration // 默认 1min
	BatchSize int           // 每次 Claim 多少, 默认 50
}

// DefaultReversalRetryConfig.
func DefaultReversalRetryConfig() ReversalRetryWorkerConfig {
	return ReversalRetryWorkerConfig{
		Interval:  1 * time.Minute,
		BatchSize: 50,
	}
}

// ReversalRetryWorker 周期扫 outbox 调 Applier 重试.
type ReversalRetryWorker struct {
	Cfg     ReversalRetryWorkerConfig
	Outbox  ReversalOutboxClaimer
	Applier ReversalApplier // 真重试用; 内部仍走 ApplyReversalAtomic
	Log     *zap.Logger
}

// Run 阻塞循环, ctx.Done 退出.
func (w *ReversalRetryWorker) Run(ctx context.Context) {
	if w.Cfg.Interval <= 0 {
		w.Cfg.Interval = 1 * time.Minute
	}
	if w.Cfg.BatchSize <= 0 {
		w.Cfg.BatchSize = 50
	}
	t := time.NewTicker(w.Cfg.Interval)
	defer t.Stop()
	if w.Log != nil {
		w.Log.Info("reversal retry worker started",
			zap.Duration("interval", w.Cfg.Interval),
			zap.Int("batch", w.Cfg.BatchSize))
	}
	for {
		select {
		case <-ctx.Done():
			if w.Log != nil {
				w.Log.Info("reversal retry worker stopped")
			}
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *ReversalRetryWorker) tick(ctx context.Context) {
	if w.Outbox == nil || w.Applier == nil {
		return
	}
	jobs, err := w.Outbox.Claim(ctx, w.Cfg.BatchSize)
	if err != nil {
		if w.Log != nil {
			w.Log.Warn("reversal retry: claim failed", zap.Error(err))
		}
		return
	}
	for _, j := range jobs {
		w.retryOne(ctx, j)
	}
}

func (w *ReversalRetryWorker) retryOne(ctx context.Context, j ReversalOutboxJob) {
	// 重建一个最小 Reversal 用 ID + Transfer + AmountMinor + 状态.
	// 真实 metadata 已落库 (insert 已经写入了, 此处只是重试 transfer.reversed_amount 累加).
	// 但 ApplyReversalAtomic 是 "原子 INSERT + UPDATE" 二合一, 重 insert 由 ON DUPLICATE KEY 幂等.
	rv := &domain.Reversal{
		ID:             j.ReversalID,
		Transfer:       j.TransferID,
		AmountMinor:    j.DeltaMinor,
		IdempotencyKey: j.ReversalID, // 跟 refund.go 中保持一致
		Status:         domain.ReversalStatusSucceeded,
	}
	if err := w.Applier.Apply(ctx, rv, j.DeltaMinor); err != nil {
		// 失败累计.
		if j.RetryCount+1 >= j.MaxRetry {
			if w.Log != nil {
				w.Log.Error("reversal retry: dead-letter (max retry)",
					zap.String("reversal_id", j.ReversalID),
					zap.Int("max_retry", j.MaxRetry), zap.Error(err))
			}
			_ = w.Outbox.MarkDeadLetter(ctx, j.ID, err.Error())
			return
		}
		if w.Log != nil {
			w.Log.Warn("reversal retry: still failing, will try later",
				zap.String("reversal_id", j.ReversalID),
				zap.Int("retry", j.RetryCount), zap.Error(err))
		}
		_ = w.Outbox.UpdateError(ctx, j.ID, err.Error())
		return
	}
	if w.Log != nil {
		w.Log.Info("reversal retry: succeeded",
			zap.String("reversal_id", j.ReversalID),
			zap.Int("retry", j.RetryCount))
	}
	_ = w.Outbox.MarkDone(ctx, j.ID)
}

// ─── refund.go 集成入口 ───────────────────────────────────────────────────────
//
// 把 outbox enqueue 暴露给 engine.HandleRefund 用. engine 上加 ReversalRetryEnqueuer 字段,
// refund.go 失败分支调它. 实现见 main.go 的 reversalOutboxAdapter.
type ReversalRetryEnqueuer interface {
	Enqueue(ctx context.Context, reversalID, transferID string, deltaMinor int64, lastErr error) error
}

// ErrReversalRetryEnqueued — refund.go 把失败 reversal 推到 outbox 后用本错误标记
// "暂时失败但已排队重试", 让上层 metric 区分 dead-letter 跟 retry-pending.
var ErrReversalRetryEnqueued = errors.New("reversal failed, enqueued for retry")

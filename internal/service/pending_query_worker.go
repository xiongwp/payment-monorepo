package service

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/metrics"
	"github.com/xiongwp/payment-channel/internal/repo"
)

// PendingQueryWorker 周期扫 acquirer_tx 里 state=unknown / state=pending（且
// older than olderThan）的行，对每行调 adapter.Query 推进到终态。
//
// 与 CallRetryWorker 的关键区别：**不重发原请求**。原请求可能已经在渠道侧
// 落账，重发就是双扣 / 双退。Query 是 idempotent 的读路径，重复调用安全。
//
// 状态机：
//
//	unknown / pending  ──Query.Result=succeeded/authorized──→  succeeded
//	unknown / pending  ──Query.Result=failed──────────────────→  failed
//	unknown / pending  ──Query.Result=processing──────────────→  unknown (bump query_count)
//	unknown / pending  ──Query 自身报错─────────────────────→  unknown (bump query_count)
//
// 节流由 ListUnknownTxs 的 queryThrottle 参数保证：单笔 unknown 行不会每个
// tick 都被查（默认 30s）。query_count 超过 maxQueryAttempts 后只记 Warn，
// 关单走 ListStuckPending → CancelStuckWorker（不在本 worker 范围）。
type PendingQueryWorker struct {
	reg           channel.Registry
	txRepo        repo.AcquirerTxRepository
	interval      time.Duration
	limit         int
	olderThan     time.Duration
	queryThrottle time.Duration
	maxAttempts   int
	logger        *zap.Logger
	now           func() time.Time
}

func NewPendingQueryWorker(
	reg channel.Registry,
	txRepo repo.AcquirerTxRepository,
	interval, olderThan, queryThrottle time.Duration,
	limit, maxAttempts int,
	logger *zap.Logger,
) *PendingQueryWorker {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if olderThan <= 0 {
		olderThan = 30 * time.Second
	}
	if queryThrottle <= 0 {
		queryThrottle = 30 * time.Second
	}
	if limit <= 0 {
		limit = 200
	}
	if maxAttempts <= 0 {
		maxAttempts = 30
	}
	return &PendingQueryWorker{
		reg:           reg,
		txRepo:        txRepo,
		interval:      interval,
		olderThan:     olderThan,
		queryThrottle: queryThrottle,
		limit:         limit,
		maxAttempts:   maxAttempts,
		logger:        logger,
		now:           time.Now,
	}
}

func (w *PendingQueryWorker) Start(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.logger.Warn("pending query tick failed", zap.Error(err))
			}
		}
	}
}

func (w *PendingQueryWorker) Tick(ctx context.Context) error {
	rows, err := w.txRepo.ListUnknownTxs(ctx, w.limit, w.olderThan, w.queryThrottle)
	if err != nil {
		return err
	}
	for _, tx := range rows {
		w.queryOne(ctx, tx)
	}
	return nil
}

func (w *PendingQueryWorker) queryOne(ctx context.Context, tx *domain.AcquirerTx) {
	ad, ok := w.reg.Get(tx.Adapter)
	if !ok {
		w.logger.Warn("pending query: adapter not registered",
			zap.String("aq_id", tx.AqID), zap.String("adapter", tx.Adapter))
		return
	}
	// extRefNo 可能为空——Charge 第一次 callErr 时根本没拿到响应。把 PiID
	// 透下去让 adapter 自己用 metadata 查；不行就只能 bump query_count 等到
	// stuck-pending worker 关单。
	req := &channel.QueryRequest{PiID: tx.PiID, ExternalRefNo: tx.ExternalRefNo}
	start := w.now()
	resp, qErr := ad.Query(ctx, req)
	metrics.AcquirerCallDuration.WithLabelValues(tx.Adapter, string(domain.ActionQuery)).Observe(time.Since(start).Seconds())

	now := w.now()
	fields := map[string]any{
		"updated_at":    now,
		"last_query_at": now,
		"query_count":   tx.QueryCount + 1,
	}
	outcome := "unknown"
	if qErr != nil {
		// Query 自身失败：依然 unknown，bump count + last_query_at 节流。
		w.logger.Info("pending query: adapter Query failed",
			zap.String("aq_id", tx.AqID),
			zap.String("adapter", tx.Adapter),
			zap.Int("query_count", tx.QueryCount+1),
			zap.Error(qErr))
	} else if resp != nil {
		// 拿到 Query 响应再更新 ext_ref_no（adapter 可能从 metadata 查到原本缺的流水号）
		if resp.ExternalRefNo != "" && tx.ExternalRefNo == "" {
			fields["external_ref_no"] = resp.ExternalRefNo
		}
		// 把 Query 响应快照存进 response_body 便于事后排查；
		// 不覆盖原 request_body（那是当初发起请求的内容）。
		if b, err := json.Marshal(resp); err == nil {
			fields["response_body"] = string(b)
		}
		switch resp.Result {
		case channel.ResultSucceeded, channel.ResultAuthorized:
			fields["state"] = string(domain.AcquirerTxSucceeded)
			outcome = "succeeded"
		case channel.ResultFailed:
			fields["state"] = string(domain.AcquirerTxFailed)
			outcome = "failed"
		case channel.ResultProcessing, channel.ResultRequiresAction, channel.ResultUnknown:
			// 渠道还没出终态——继续 unknown，下次 tick 再查。
		default:
			// 未知 Result：保守留在 unknown。
		}
	}

	if outcome == "unknown" && tx.QueryCount+1 >= w.maxAttempts {
		w.logger.Warn("pending query: max attempts reached, leaving for stuck-pending sweeper",
			zap.String("aq_id", tx.AqID),
			zap.String("adapter", tx.Adapter),
			zap.Int("query_count", tx.QueryCount+1))
	}

	if err := w.txRepo.UpdateResult(ctx, tx.PiID, tx.ID, fields); err != nil {
		w.logger.Warn("pending query: update failed",
			zap.String("aq_id", tx.AqID), zap.Error(err))
		return
	}
	metrics.AcquirerCallTotal.WithLabelValues(tx.Adapter, string(domain.ActionQuery), outcome).Inc()
}

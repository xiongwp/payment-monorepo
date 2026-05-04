package service

import (
	"context"
	"encoding/json"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/payment-channel/internal/channel"
	"github.com/xiongwp/payment-channel/internal/domain"
	"github.com/xiongwp/payment-channel/internal/repo"
)

// CallRetryWorker 扫 acquirer_tx 里 state=failed 且 next_retry_at<=now 的行，
// 按其 action 类型重放（仍然靠幂等表避免重复下单）。
type CallRetryWorker struct {
	svc      *AcquirerService
	txRepo   repo.AcquirerTxRepository
	interval time.Duration
	limit    int
	logger   *zap.Logger
}

func NewCallRetryWorker(svc *AcquirerService, txRepo repo.AcquirerTxRepository, interval time.Duration, limit int, logger *zap.Logger) *CallRetryWorker {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if limit <= 0 {
		limit = 200
	}
	return &CallRetryWorker{svc: svc, txRepo: txRepo, interval: interval, limit: limit, logger: logger}
}

func (w *CallRetryWorker) Start(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.logger.Warn("call retry tick failed", zap.Error(err))
			}
		}
	}
}

func (w *CallRetryWorker) Tick(ctx context.Context) error {
	rows, err := w.txRepo.ListPendingRetries(ctx, w.limit)
	if err != nil {
		return err
	}
	for _, tx := range rows {
		w.replayOne(ctx, tx)
	}
	return nil
}

func (w *CallRetryWorker) replayOne(ctx context.Context, tx *domain.AcquirerTx) {
	w.logger.Info("call retry replay",
		zap.String("aq_id", tx.AqID),
		zap.String("adapter", tx.Adapter),
		zap.String("action", string(tx.Action)),
		zap.Int("attempt", tx.Attempt))

	switch tx.Action {
	case domain.ActionCharge:
		var req channel.ChargeRequest
		_ = json.Unmarshal([]byte(tx.RequestBody), &req)
		_, _ = w.svc.Charge(ctx, tx.Adapter, &req)
	case domain.ActionRefund:
		var req channel.RefundRequest
		_ = json.Unmarshal([]byte(tx.RequestBody), &req)
		_, _ = w.svc.Refund(ctx, tx.Adapter, &req)
	case domain.ActionCapture:
		var req channel.CaptureRequest
		_ = json.Unmarshal([]byte(tx.RequestBody), &req)
		_, _ = w.svc.Capture(ctx, tx.Adapter, &req)
	case domain.ActionVoid:
		var req channel.VoidRequest
		_ = json.Unmarshal([]byte(tx.RequestBody), &req)
		_, _ = w.svc.Void(ctx, tx.Adapter, &req)
	}
}

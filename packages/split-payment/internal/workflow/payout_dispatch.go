// payout_dispatch.go — SP-FIN-2 把 pending Payout 推到 clearing-settlement 走银行通道.
//
// 状态机:
//
//   pending  ── PayoutDispatcher 调 clearing API ──> in_transit
//   in_transit ── 银行回调 / poll status ──────────> paid / failed
//
// 本 worker 只负责 pending → in_transit 这一步; in_transit → paid 由 clearing-settlement
// 服务的回调 webhook 触发 (Phase 4 加 webhook 入口端点).
//
// 设计:
//   - 每 30s poll 一次 status='pending' 的 payout (limit 50)
//   - 对每条调 ClearingClient.DispatchPayout(payout) → 拿到 bank_ref + arrival_date
//   - 成功: UpdateStatus(id, 'in_transit', arrival_date)
//   - 失败 (网络/拒) 重试 3 次, 还失败 → status='failed' + failure_code
//
// 限流: 单 worker 单线程顺序处理, 不并发避免过载下游.
package workflow

import (
	"context"
	"time"

	"github.com/xiongwp/split-payment/internal/domain"

	"go.uber.org/zap"
)

// ClearingClient 调 clearing-settlement 服务的抽象.
//
// 真实实现走 gRPC / HTTP 到 clearing-settlement 服务.
type ClearingClient interface {
	DispatchPayout(ctx context.Context, p *domain.Payout) (*DispatchResult, error)
}

// DispatchResult clearing 返回.
type DispatchResult struct {
	BankRef     string    // 银行通道发回的引用号
	ArrivalDate time.Time // 预计到账
	Status      string    // 通常 "in_transit"
	FailureCode string
	FailureMsg  string
}

// NoopClearingClient 不真发, 仅 log → 标 in_transit. dev 用.
type NoopClearingClient struct{ Log *zap.Logger }

// DispatchPayout.
func (n NoopClearingClient) DispatchPayout(_ context.Context, p *domain.Payout) (*DispatchResult, error) {
	if n.Log != nil {
		n.Log.Info("noop payout dispatch (no real bank)",
			zap.String("payout_id", p.ID),
			zap.Int64("amount", p.AmountMinor))
	}
	return &DispatchResult{
		BankRef:     "noop_" + p.ID,
		ArrivalDate: time.Now().UTC().AddDate(0, 0, 2),
		Status:      domain.PayoutStatusInTransit,
	}, nil
}

// PendingPayoutsRepo worker 用 — 扫 pending payout + 更新状态.
type PendingPayoutsRepo interface {
	ListPending(ctx context.Context, limit int) ([]*domain.Payout, error)
	UpdateStatus(ctx context.Context, id, status, failureCode, failureMsg string, arrivalAt time.Time) error
}

// PayoutDispatchConfig.
type PayoutDispatchConfig struct {
	Interval   time.Duration // 默认 30s
	BatchLimit int           // 默认 50
	MaxRetries int           // 默认 3
}

// DefaultPayoutDispatchConfig.
func DefaultPayoutDispatchConfig() PayoutDispatchConfig {
	return PayoutDispatchConfig{Interval: 30 * time.Second, BatchLimit: 50, MaxRetries: 3}
}

// PayoutDispatchWorker.
type PayoutDispatchWorker struct {
	Cfg      PayoutDispatchConfig
	Payouts  PendingPayoutsRepo
	Clearing ClearingClient
	Events   EventPublisher
	Log      *zap.Logger
}

// Run 阻塞 ticker.
func (w *PayoutDispatchWorker) Run(ctx context.Context) {
	if w.Cfg.Interval <= 0 {
		w.Cfg.Interval = 30 * time.Second
	}
	t := time.NewTicker(w.Cfg.Interval)
	defer t.Stop()
	w.Log.Info("payout dispatch worker started",
		zap.Duration("interval", w.Cfg.Interval))
	for {
		select {
		case <-ctx.Done():
			w.Log.Info("payout dispatch worker stopped")
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *PayoutDispatchWorker) tick(ctx context.Context) {
	if w.Payouts == nil || w.Clearing == nil {
		return
	}
	pending, err := w.Payouts.ListPending(ctx, w.Cfg.BatchLimit)
	if err != nil {
		w.Log.Warn("payout dispatch: list failed", zap.Error(err))
		return
	}
	if len(pending) == 0 {
		return
	}
	for _, p := range pending {
		var (
			res *DispatchResult
			err error
		)
		for i := 0; i < w.Cfg.MaxRetries; i++ {
			res, err = w.Clearing.DispatchPayout(ctx, p)
			if err == nil {
				break
			}
			w.Log.Warn("payout dispatch attempt failed",
				zap.String("payout_id", p.ID),
				zap.Int("attempt", i+1),
				zap.Error(err))
			time.Sleep(time.Duration(i+1) * time.Second)
		}
		if err != nil {
			_ = w.Payouts.UpdateStatus(ctx, p.ID,
				domain.PayoutStatusFailed,
				"dispatch_failed", err.Error(), time.Time{})
			if w.Events != nil {
				_ = w.Events.Publish(ctx, EventPayoutFailed, p)
			}
			continue
		}
		// 成功
		newStatus := res.Status
		if newStatus == "" {
			newStatus = domain.PayoutStatusInTransit
		}
		_ = w.Payouts.UpdateStatus(ctx, p.ID,
			newStatus, res.FailureCode, res.FailureMsg, res.ArrivalDate)
		p.Status = newStatus
		p.ArrivalDate = res.ArrivalDate
		if w.Events != nil {
			_ = w.Events.Publish(ctx, EventPayoutInTransit, p)
		}
		w.Log.Info("payout dispatched",
			zap.String("payout_id", p.ID),
			zap.String("bank_ref", res.BankRef),
			zap.Time("arrival", res.ArrivalDate))
	}
}

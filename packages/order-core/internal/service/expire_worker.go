package service

import (
	"context"
	"time"

	"github.com/xiongwp/payment-util/trace"
	"go.uber.org/zap"
)

// ExpireWorker 周期性扫描 expired_at < now 的非终态 PI 并关单。
type ExpireWorker struct {
	svc      PaymentIntentService
	interval time.Duration
	limit    int
	logger   *zap.Logger
}

// NewExpireWorker 构造（默认间隔 60s / 每次 200 条）
func NewExpireWorker(svc PaymentIntentService, interval time.Duration, limit int, logger *zap.Logger) *ExpireWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	if limit <= 0 {
		limit = 200
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ExpireWorker{svc: svc, interval: interval, limit: limit, logger: logger}
}

// Start 阻塞运行直到 ctx 取消
func (w *ExpireWorker) Start(ctx context.Context) {
	w.logger.Info("expire worker started",
		zap.Duration("interval", w.interval), zap.Int("limit", w.limit))
	t := time.NewTicker(w.interval)
	defer t.Stop()

	runOnce := func() {
		// 每次扫过期单都起 background ctx：trace_id 新发 + shadow=false。
		// 防止 expire 路径若意外读到 shadow ctx 把 *_shadow 单 mark 到主表。
		bgCtx, cancel := trace.NewBackground(ctx, "expire-worker", w.logger, w.interval)
		defer cancel()
		if _, err := w.svc.ExpireOverdue(bgCtx, w.limit); err != nil {
			trace.Logger(bgCtx, w.logger).Warn("expire sweep failed", zap.Error(err))
		}
	}
	runOnce() // 启动先跑一次
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("expire worker stopped")
			return
		case <-t.C:
			runOnce()
		}
	}
}

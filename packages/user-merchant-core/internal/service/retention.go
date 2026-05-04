package service

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/xiongwp/user-merchant-core/internal/repo"
)

// RetentionSweeper 定时真删软删且超过保留期的商户。
// 合规默认 7 年（PCI 审计 / 账务追溯）。scan interval 一般 24h。
type RetentionSweeper struct {
	repo      repo.MerchantRepository
	retention time.Duration
	interval  time.Duration
	logger    *zap.Logger
}

// NewRetentionSweeper 构造。retention/interval <=0 取默认（7y / 24h）。
func NewRetentionSweeper(r repo.MerchantRepository, retention, interval time.Duration, logger *zap.Logger) *RetentionSweeper {
	if retention <= 0 {
		retention = 7 * 365 * 24 * time.Hour
	}
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RetentionSweeper{repo: r, retention: retention, interval: interval, logger: logger}
}

// Start 阻塞直到 ctx 取消。kill 路径：main 的 ctx 被 cancel。
func (s *RetentionSweeper) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	// 启动立即跑一次；避免重启后第一次要等一整个 interval。
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *RetentionSweeper) sweep(ctx context.Context) {
	cutoff := time.Now().Add(-s.retention)
	for {
		n, err := s.repo.PurgeDeletedBefore(ctx, cutoff)
		if err != nil {
			s.logger.Warn("retention sweep failed", zap.Error(err))
			return
		}
		if n == 0 {
			return
		}
		s.logger.Info("retention sweep batch purged",
			zap.Int64("rows", n),
			zap.Time("cutoff", cutoff))
		// 继续下一批，直到没得删
	}
}

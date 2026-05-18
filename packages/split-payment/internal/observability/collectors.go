// collectors.go — SP-AC-7 O2: 周期性把 sql.DB.Stats / outbox table 状态推 Prometheus.
//
// 区别于 promauto Counter/Histogram (业务路径递增): 这些是 gauge, 必须独立 goroutine
// 定时 SELECT COUNT(*) 把数推过来. 30s 一次, 资源消耗可忽略.
package observability

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/zap"
)

// StartDBStatsCollector 每 N 秒 push 一次 sql.DB.Stats().
// ctx.Done 退出.
func StartDBStatsCollector(ctx context.Context, db *sql.DB, interval time.Duration, log *zap.Logger) {
	if db == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				stats := db.Stats()
				DBOpenConnections.Set(float64(stats.OpenConnections))
				DBInUseConnections.Set(float64(stats.InUse))
				DBWaitCount.Set(float64(stats.WaitCount))
			}
		}
	}()
	if log != nil {
		log.Info("DB stats collector started", zap.Duration("interval", interval))
	}
}

// OutboxStat 跟 main.go 解耦 (避免循环依赖); main.go 注册具体 outbox 名字 + scrape func.
type OutboxStat struct {
	PendingDepth     int64
	DeadLetterCount  int64
	OldestAgeSeconds int64
}

// OutboxScraper 单条 outbox 的快照函数; main.go 用 sql.DB SELECT COUNT/MAX 实现.
type OutboxScraper func(context.Context) (OutboxStat, error)

// StartOutboxMetricsCollector 周期把 OutboxStat 推到 Prometheus.
//
// scrapers map: name → scraper. e.g. "event_outbox" / "reversal_retry_outbox".
func StartOutboxMetricsCollector(ctx context.Context, scrapers map[string]OutboxScraper, interval time.Duration, log *zap.Logger) {
	if len(scrapers) == 0 {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for name, scrape := range scrapers {
					stat, err := scrape(ctx)
					if err != nil {
						if log != nil {
							log.Warn("outbox scrape failed",
								zap.String("outbox", name), zap.Error(err))
						}
						continue
					}
					OutboxDepth.WithLabelValues(name).Set(float64(stat.PendingDepth))
					OutboxDeadLetter.WithLabelValues(name).Set(float64(stat.DeadLetterCount))
					OutboxOldestAgeSeconds.WithLabelValues(name).Set(float64(stat.OldestAgeSeconds))
				}
			}
		}
	}()
	if log != nil {
		log.Info("outbox metrics collector started",
			zap.Int("count", len(scrapers)),
			zap.Duration("interval", interval))
	}
}

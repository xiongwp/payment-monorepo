// runtime.go — Go runtime / DB / outbox 通用 metric collector.
//
// 用法 (启动期一次):
//	obsbootstrap.StartRuntimeCollectors(ctx, obsbootstrap.RuntimeOpts{
//	    ServiceName: "payment-core",
//	    DB:          db,
//	    OutboxScrapers: map[string]obsbootstrap.OutboxScraper{
//	        "event_outbox": myScraper,
//	    },
//	    Interval: 30 * time.Second,
//	})
//
// runtime metrics: goroutine_count / gc_pause / heap_alloc — Go SDK 自带的 promauto
// 已经把 runtime stats 注册到默认 registry, 这里只补 DB / outbox.
package obsbootstrap

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// 通用 DB pool metric (label 区分多个 *sql.DB, e.g. "primary"/"meta"/"shard_0").
var (
	dbOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "db",
		Name:      "open_connections",
		Help:      "DB pool open connections.",
	}, []string{"service", "pool"})
	dbInUse = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "db",
		Name:      "in_use_connections",
		Help:      "DB pool in-use connections.",
	}, []string{"service", "pool"})
	dbWaitCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "db",
		Name:      "wait_count_total",
		Help:      "Cumulative number of times a connection was waited for.",
	}, []string{"service", "pool"})
)

// OutboxStat 通用 outbox 快照.
type OutboxStat struct {
	PendingDepth     int64
	DeadLetterCount  int64
	OldestAgeSeconds int64
}

// OutboxScraper 单条 outbox 的 SELECT COUNT/MIN 包装.
type OutboxScraper func(context.Context) (OutboxStat, error)

// 通用 outbox metric.
var (
	outboxDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "outbox",
		Name:      "depth",
		Help:      "Outbox pending depth per (service, name).",
	}, []string{"service", "name"})
	outboxDLQ = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "outbox",
		Name:      "dead_letter_total",
		Help:      "Outbox dead_letter row count.",
	}, []string{"service", "name"})
	outboxAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "obs",
		Subsystem: "outbox",
		Name:      "oldest_age_seconds",
		Help:      "Age in seconds of oldest pending row.",
	}, []string{"service", "name"})
)

// RuntimeOpts.
type RuntimeOpts struct {
	ServiceName string
	// DBs map[poolName]*sql.DB — 每个 pool 独立 label.
	DBs map[string]*sql.DB
	// OutboxScrapers map[outboxName]scraper.
	OutboxScrapers map[string]OutboxScraper
	// Interval 拉取间隔, 默认 30s.
	Interval time.Duration
	Logger   *zap.Logger
}

// StartRuntimeCollectors 起一个 goroutine 周期性把 DB stats + outbox 状态推到 Prometheus.
// ctx.Done 退出.
func StartRuntimeCollectors(ctx context.Context, opts RuntimeOpts) {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.ServiceName == "" {
		opts.ServiceName = "unknown"
	}
	go func() {
		t := time.NewTicker(opts.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// DB stats
				for pool, db := range opts.DBs {
					if db == nil {
						continue
					}
					s := db.Stats()
					dbOpen.WithLabelValues(opts.ServiceName, pool).Set(float64(s.OpenConnections))
					dbInUse.WithLabelValues(opts.ServiceName, pool).Set(float64(s.InUse))
					dbWaitCount.WithLabelValues(opts.ServiceName, pool).Set(float64(s.WaitCount))
				}
				// outbox stats
				for name, scrape := range opts.OutboxScrapers {
					stat, err := scrape(ctx)
					if err != nil {
						if opts.Logger != nil {
							opts.Logger.Warn("obs outbox scrape failed",
								zap.String("name", name), zap.Error(err))
						}
						continue
					}
					outboxDepth.WithLabelValues(opts.ServiceName, name).Set(float64(stat.PendingDepth))
					outboxDLQ.WithLabelValues(opts.ServiceName, name).Set(float64(stat.DeadLetterCount))
					outboxAge.WithLabelValues(opts.ServiceName, name).Set(float64(stat.OldestAgeSeconds))
				}
			}
		}
	}()
	if opts.Logger != nil {
		opts.Logger.Info("runtime collectors started",
			zap.String("service", opts.ServiceName),
			zap.Duration("interval", opts.Interval),
			zap.Int("dbs", len(opts.DBs)),
			zap.Int("outboxes", len(opts.OutboxScrapers)))
	}
}

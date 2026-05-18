// data-rights cmd/server — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// Env:
//
//	DR_ADDR              默认 ":8091"
//	DR_ADMIN_TOKEN       /admin/* 鉴权
//	DR_OVERDUE_CRON_SEC  扫 overdue 工单的周期 (默认 3600s = 1h)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/adminhttp"
	"reconcile-system/packages/data-rights/internal/audit"
	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/metrics"
	"reconcile-system/packages/data-rights/internal/orchestrator"
	"reconcile-system/packages/data-rights/internal/store"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newStore,
			newOrchestrator,
			newAuditSink,
			newPromRegistry,
			newAdminServer,
			newHTTPServer,
		),
		fx.Invoke(
			startHTTPServer,
			startOverdueScanner,
		),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: log.Named("fx")}
		}),
	).Run()
}

func newLogger(lc fx.Lifecycle) (*zap.Logger, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("zap build: %w", err)
	}
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { _ = logger.Sync(); return nil }})
	return logger, nil
}

func newStore() *store.MemStore { return store.NewMemStore() }

func newOrchestrator(s *store.MemStore, log *zap.Logger) *orchestrator.Orchestrator {
	return orchestrator.New(s, orchestrator.DefaultRegistry(), log)
}

func newAuditSink(lc fx.Lifecycle, log *zap.Logger) *audit.HTTPSink {
	sink := audit.NewHTTPSink(audit.HTTPConfig{
		BaseURL: os.Getenv("AUDITLOG_URL"),
		Token:   os.Getenv("AUDITLOG_TOKEN"),
		Service: "data-rights",
	}, log)
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { sink.Stop(); return nil }})
	return sink
}

func newPromRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)
	return reg
}

func newAdminServer(s *store.MemStore, orch *orchestrator.Orchestrator, sink *audit.HTTPSink, log *zap.Logger) *adminhttp.Server {
	return &adminhttp.Server{
		Store:      s,
		Orch:       orch,
		Audit:      sink,
		AdminToken: os.Getenv("DR_ADMIN_TOKEN"),
		Log:        log,
	}
}

func newHTTPServer(srv *adminhttp.Server, reg *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())
	return &http.Server{
		Addr:              getenv("DR_ADDR", ":8091"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("data-rights listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("server failed", zap.String("addr", srv.Addr), zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return srv.Shutdown(shCtx)
		},
	})
}

func startOverdueScanner(lc fx.Lifecycle, s store.Store, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	cronSec, _ := strconv.Atoi(os.Getenv("DR_OVERDUE_CRON_SEC"))
	if cronSec <= 0 {
		cronSec = 3600
	}
	interval := time.Duration(cronSec) * time.Second
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go scanOverdue(ctx, s, interval, log)
			log.Info("overdue scanner started", zap.Duration("interval", interval))
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func scanOverdue(ctx context.Context, s store.Store, interval time.Duration, log *zap.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			overdue, _ := s.ListRequests(store.ListFilter{Overdue: true, Limit: 1000})
			metrics.OverdueGauge.Set(float64(len(overdue)))
			if len(overdue) > 0 {
				log.Warn("data-rights overdue requests",
					zap.Int("count", len(overdue)),
					zap.String("first", overdue[0].RequestID))
			}
			byState := map[domain.State]int{}
			all, _ := s.ListRequests(store.ListFilter{Limit: 10000})
			for _, r := range all {
				byState[r.State]++
			}
			for state, n := range byState {
				metrics.RequestsByState.WithLabelValues(string(state)).Set(float64(n))
			}
		}
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

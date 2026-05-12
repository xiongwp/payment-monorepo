// data-rights cmd/server — 入口.
//
// Env:
//   DR_ADDR              默认 ":8091"
//   DR_ADMIN_TOKEN       /admin/* 鉴权
//   DR_OVERDUE_CRON_SEC  扫 overdue 工单的周期 (默认 3600s = 1h)

package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/adminhttp"
	"reconcile-system/packages/data-rights/internal/audit"
	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/metrics"
	"reconcile-system/packages/data-rights/internal/orchestrator"
	"reconcile-system/packages/data-rights/internal/store"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	mem := store.NewMemStore()
	registry := orchestrator.DefaultRegistry()
	orch := orchestrator.New(mem, registry, log)

	auditSink := audit.NewHTTPSink(audit.HTTPConfig{
		BaseURL: os.Getenv("AUDITLOG_URL"),
		Token:   os.Getenv("AUDITLOG_TOKEN"),
		Service: "data-rights",
	}, log)
	defer auditSink.Stop()

	srv := &adminhttp.Server{
		Store:      mem,
		Orch:       orch,
		Audit:      auditSink,
		AdminToken: os.Getenv("DR_ADMIN_TOKEN"),
		Log:        log,
	}

	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())

	addr := getenv("DR_ADDR", ":8091")
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// overdue scanner — 每小时检查超 30 天工单, 报 metric + 通知 ops
	cronSec, _ := strconv.Atoi(os.Getenv("DR_OVERDUE_CRON_SEC"))
	if cronSec <= 0 {
		cronSec = 3600
	}
	go scanOverdue(ctx, mem, time.Duration(cronSec)*time.Second, log)

	go func() {
		log.Info("data-rights listening", zap.String("addr", addr))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down...")
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = httpSrv.Shutdown(shCtx)
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
			// 状态机指标
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

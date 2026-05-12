// aml-screening cmd/server — 入口.
//
// Env:
//   AML_ADDR           监听地址, 默认 ":8088"
//   AML_ADMIN_TOKEN    /admin/* 鉴权 token (空则不校验, 仅 dev)
//   AML_DEV_SEED       1 = 启动后灌测试名单 (dev), 默认 0
//   AML_REFRESH_OFAC   1 = 启动 OFAC SDN 24h 周期刷新, 默认 0 (拉外网)
//   AML_BLOCK_THRESH   默认 90
//   AML_REVIEW_THRESH  默认 70

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

	"reconcile-system/packages/aml-screening/internal/adminhttp"
	"reconcile-system/packages/aml-screening/internal/audit"
	"reconcile-system/packages/aml-screening/internal/domain"
	"reconcile-system/packages/aml-screening/internal/metrics"
	"reconcile-system/packages/aml-screening/internal/screening"
	"reconcile-system/packages/aml-screening/internal/sources"
	"reconcile-system/packages/aml-screening/internal/store"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	cfg := screening.DefaultConfig()
	if v := getEnvInt("AML_BLOCK_THRESH"); v > 0 {
		cfg.BlockThreshold = v
	}
	if v := getEnvInt("AML_REVIEW_THRESH"); v > 0 {
		cfg.ReviewThreshold = v
	}

	mem := store.NewMemStore()

	if os.Getenv("AML_DEV_SEED") == "1" {
		if err := sources.SeedDev(mem); err != nil {
			log.Fatal("seed dev", zap.Error(err))
		}
		log.Info("aml-screening seeded dev entries")
	}

	// metrics
	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)

	// audit — AUDITLOG_URL 设了走 HTTPSink (真接 audit-log), 否则降级 LogSink
	auditSink := audit.NewHTTPSink(audit.HTTPConfig{
		BaseURL: os.Getenv("AUDITLOG_URL"),
		Token:   os.Getenv("AUDITLOG_TOKEN"),
		Service: "aml-screening",
	}, log)
	defer auditSink.Stop()

	// refreshers (按 env 开关)
	refreshers := map[domain.ListSource]*sources.Refresher{}
	if os.Getenv("AML_REFRESH_OFAC") == "1" {
		fetcher := sources.NewOFACFetcher()
		refreshers[domain.SourceOFACSDN] = &sources.Refresher{
			Source:   domain.SourceOFACSDN,
			Interval: 24 * time.Hour,
			Fetch:    fetcher.Fetch,
			Parse:    sources.ParseOFACSDN,
			Store:    mem,
			Log:      log,
		}
	}

	srv := &adminhttp.Server{
		Store:      mem,
		Cfg:        cfg,
		Audit:      auditSink,
		AdminToken: os.Getenv("AML_ADMIN_TOKEN"),
		Log:        log,
		Refreshers: refreshers,
	}

	addr := os.Getenv("AML_ADDR")
	if addr == "" {
		addr = ":8088"
	}

	// admin / business 一个端口; metrics 分独立端口避免被外网爆探测
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// refreshers go
	for _, r := range refreshers {
		go r.Loop(ctx)
	}

	go func() {
		log.Info("aml-screening listening", zap.String("addr", addr))
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

func getEnvInt(k string) int {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

// aml-screening cmd/server — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// Env:
//
//	AML_ADDR           监听地址, 默认 ":8088"
//	AML_ADMIN_TOKEN    /admin/* 鉴权 token (空则不校验, 仅 dev)
//	AML_DEV_SEED       1 = 启动后灌测试名单 (dev), 默认 0
//	AML_REFRESH_OFAC   1 = 启动 OFAC SDN 24h 周期刷新, 默认 0 (拉外网)
//	AML_BLOCK_THRESH   默认 90
//	AML_REVIEW_THRESH  默认 70
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

	"reconcile-system/packages/aml-screening/internal/adminhttp"
	"reconcile-system/packages/aml-screening/internal/audit"
	"reconcile-system/packages/aml-screening/internal/domain"
	"reconcile-system/packages/aml-screening/internal/metrics"
	"reconcile-system/packages/aml-screening/internal/screening"
	"reconcile-system/packages/aml-screening/internal/sources"
	"reconcile-system/packages/aml-screening/internal/store"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newScreeningCfg,
			newStore,
			newPromRegistry,
			newAuditSink,
			newRefreshers,
			newAdminServer,
			newHTTPServer,
		),
		fx.Invoke(
			startRefreshers,
			startHTTPServer,
			seedDevIfRequested,
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

func newScreeningCfg() screening.Config {
	cfg := screening.DefaultConfig()
	if v := getEnvInt("AML_BLOCK_THRESH"); v > 0 {
		cfg.BlockThreshold = v
	}
	if v := getEnvInt("AML_REVIEW_THRESH"); v > 0 {
		cfg.ReviewThreshold = v
	}
	return cfg
}

func newStore() *store.MemStore { return store.NewMemStore() }

func newPromRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)
	return reg
}

func newAuditSink(lc fx.Lifecycle, log *zap.Logger) *audit.HTTPSink {
	sink := audit.NewHTTPSink(audit.HTTPConfig{
		BaseURL: os.Getenv("AUDITLOG_URL"),
		Token:   os.Getenv("AUDITLOG_TOKEN"),
		Service: "aml-screening",
	}, log)
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { sink.Stop(); return nil }})
	return sink
}

// newRefreshers 按 env 开关 OFAC SDN 周期刷新.
func newRefreshers(mem *store.MemStore, log *zap.Logger) map[domain.ListSource]*sources.Refresher {
	out := map[domain.ListSource]*sources.Refresher{}
	if os.Getenv("AML_REFRESH_OFAC") == "1" {
		fetcher := sources.NewOFACFetcher()
		out[domain.SourceOFACSDN] = &sources.Refresher{
			Source:   domain.SourceOFACSDN,
			Interval: 24 * time.Hour,
			Fetch:    fetcher.Fetch,
			Parse:    sources.ParseOFACSDN,
			Store:    mem,
			Log:      log,
		}
	}
	return out
}

func newAdminServer(mem *store.MemStore, cfg screening.Config, sink *audit.HTTPSink,
	refreshers map[domain.ListSource]*sources.Refresher, log *zap.Logger) *adminhttp.Server {
	return &adminhttp.Server{
		Store:      mem,
		Cfg:        cfg,
		Audit:      sink,
		AdminToken: os.Getenv("AML_ADMIN_TOKEN"),
		Log:        log,
		Refreshers: refreshers,
	}
}

func newHTTPServer(srv *adminhttp.Server, reg *prometheus.Registry) *http.Server {
	addr := os.Getenv("AML_ADDR")
	if addr == "" {
		addr = ":8088"
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// startRefreshers OFAC 刷新 goroutine (有的话).
func startRefreshers(lc fx.Lifecycle, refreshers map[domain.ListSource]*sources.Refresher, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			for _, r := range refreshers {
				go r.Loop(ctx)
			}
			if len(refreshers) > 0 {
				log.Info("aml refreshers started", zap.Int("count", len(refreshers)))
			}
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("aml-screening listening", zap.String("addr", srv.Addr))
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

// seedDevIfRequested AML_DEV_SEED=1 时灌测试名单.
func seedDevIfRequested(mem *store.MemStore, log *zap.Logger) {
	if os.Getenv("AML_DEV_SEED") != "1" {
		return
	}
	if err := sources.SeedDev(mem); err != nil {
		log.Fatal("seed dev failed", zap.Error(err))
	}
	log.Info("aml-screening seeded dev entries")
}

func getEnvInt(k string) int {
	v := os.Getenv(k)
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

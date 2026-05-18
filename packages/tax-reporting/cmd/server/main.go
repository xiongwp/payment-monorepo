// tax-reporting cmd/server — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// Env:
//
//	TAX_ADDR                  默认 ":8090"
//	TAX_ADMIN_TOKEN           /admin/* 鉴权
//	TAX_FILER_NAME            平台法人名 (出现在 1099-K Issuer)
//	TAX_FILER_TIN             平台 EIN
//	TAX_FILER_ADDRESS_LINE1   /CITY/STATE/ZIP
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"reconcile-system/packages/tax-reporting/internal/adminhttp"
	"reconcile-system/packages/tax-reporting/internal/aggregator"
	"reconcile-system/packages/tax-reporting/internal/domain"
	"reconcile-system/packages/tax-reporting/internal/efile"
	"reconcile-system/packages/tax-reporting/internal/forms"
	"reconcile-system/packages/tax-reporting/internal/metrics"
	"reconcile-system/packages/tax-reporting/internal/store"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newStore,
			newAggregator,
			newFiler,
			newAdminServer,
			newPromRegistry,
			newHTTPServer,
		),
		fx.Invoke(startHTTPServer),
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

func newStore() *store.MemStore {
	return store.NewMemStore()
}

func newAggregator(s *store.MemStore) *aggregator.Aggregator {
	return aggregator.New(s)
}

func newFiler() forms.Filer {
	return forms.Filer{
		Name: getenv("TAX_FILER_NAME", "Payment Platform Inc."),
		TIN:  getenv("TAX_FILER_TIN", "00-0000000"),
		Address: domain.Address{
			Line1:   getenv("TAX_FILER_ADDRESS_LINE1", "1 Market St"),
			City:    getenv("TAX_FILER_ADDRESS_CITY", "San Francisco"),
			State:   getenv("TAX_FILER_ADDRESS_STATE", "CA"),
			Zip:     getenv("TAX_FILER_ADDRESS_ZIP", "94105"),
			Country: "US",
		},
	}
}

func newAdminServer(s *store.MemStore, agg *aggregator.Aggregator, filer forms.Filer, log *zap.Logger) *adminhttp.Server {
	return &adminhttp.Server{
		Store:      s,
		Agg:        agg,
		Filer:      filer,
		Submitter:  efile.StubSubmitter{},
		Thresholds: domain.DefaultThresholds(),
		AdminToken: os.Getenv("TAX_ADMIN_TOKEN"),
		Log:        log,
	}
}

func newPromRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)
	return reg
}

func newHTTPServer(srv *adminhttp.Server, reg *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())
	addr := getenv("TAX_ADDR", ":8090")
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("tax-reporting listening", zap.String("addr", srv.Addr))
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
			if err := srv.Shutdown(shCtx); err != nil {
				log.Warn("shutdown error", zap.Error(err))
				return err
			}
			return nil
		},
	})
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

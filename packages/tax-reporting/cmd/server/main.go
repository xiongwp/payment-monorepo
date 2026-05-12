// tax-reporting cmd/server — 入口.
//
// Env:
//   TAX_ADDR          默认 ":8090"
//   TAX_ADMIN_TOKEN   /admin/* 鉴权
//   TAX_FILER_NAME    平台法人名 (出现在 1099-K Issuer)
//   TAX_FILER_TIN     平台 EIN
//   TAX_FILER_ADDRESS_LINE1 / CITY / STATE / ZIP

package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
	log, _ := zap.NewProduction()
	defer log.Sync()

	mem := store.NewMemStore()
	agg := aggregator.New(mem)

	filer := forms.Filer{
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

	srv := &adminhttp.Server{
		Store:      mem,
		Agg:        agg,
		Filer:      filer,
		Submitter:  efile.StubSubmitter{},
		Thresholds: domain.DefaultThresholds(),
		AdminToken: os.Getenv("TAX_ADMIN_TOKEN"),
		Log:        log,
	}

	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())

	addr := getenv("TAX_ADDR", ":8090")
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("tax-reporting listening", zap.String("addr", addr))
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

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

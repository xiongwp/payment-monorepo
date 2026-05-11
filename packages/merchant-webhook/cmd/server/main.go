package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/merchant-webhook/internal/adminhttp"
	"reconcile-system/packages/merchant-webhook/internal/dispatcher"
	"reconcile-system/packages/merchant-webhook/internal/repository"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := envOr("MERCHANT_WEBHOOK_HTTP_PORT", "8080")

	repo := repository.NewMemoryRepo()
	disp := dispatcher.New(repo, "2026-05-01", logger)

	mux := http.NewServeMux()
	api := adminhttp.New(repo, disp, logger)
	api.Mount(mux)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 后台 worker: 每秒扫 ready_to_deliver 串行投递
	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, c := context.WithTimeout(ctx, 30*time.Second)
				if n, err := disp.DeliverPending(rctx, 50); err == nil && n > 0 {
					logger.Info("webhook batch delivered", zap.Int("count", n))
				}
				c()
			}
		}
	}()

	logger.Info("merchant-webhook listening", zap.String("addr", srv.Addr))
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("listen", zap.Error(err))
		}
	}()
	<-ctx.Done()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

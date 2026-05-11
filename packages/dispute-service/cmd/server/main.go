package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/dispute-service/internal/adminhttp"
	"reconcile-system/packages/dispute-service/internal/repository"
	"reconcile-system/packages/dispute-service/internal/workflow"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := envOr("DISPUTE_HTTP_PORT", "8080")

	repo := repository.NewMemoryRepo()
	svc := workflow.New(repo, repository.LogNotifier{}, logger)

	mux := http.NewServeMux()
	api := adminhttp.New(svc, repo, logger)
	api.Mount(mux)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 后台 cron: 每 10min 跑 ExpireOverdue
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, c := context.WithTimeout(ctx, 5*time.Minute)
				if n, err := svc.ExpireOverdue(rctx, 100); err == nil && n > 0 {
					logger.Info("dispute auto-expired", zap.Int("count", n))
				}
				c()
			}
		}
	}()

	logger.Info("dispute-service listening", zap.String("addr", srv.Addr))
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

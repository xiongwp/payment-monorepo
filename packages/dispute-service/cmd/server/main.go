// dispute-service 入口 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"reconcile-system/packages/dispute-service/internal/adminhttp"
	"reconcile-system/packages/dispute-service/internal/repository"
	"reconcile-system/packages/dispute-service/internal/workflow"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newRepo,
			newWorkflow,
			newAdminAPI,
			newHTTPServer,
		),
		fx.Invoke(
			startHTTPServer,
			startExpireOverdueCron,
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

func newRepo() *repository.MemoryRepo {
	return repository.NewMemoryRepo()
}

func newWorkflow(repo *repository.MemoryRepo, log *zap.Logger) *workflow.Service {
	return workflow.New(repo, repository.LogNotifier{}, log)
}

func newAdminAPI(svc *workflow.Service, repo *repository.MemoryRepo, log *zap.Logger) *adminhttp.Server {
	return adminhttp.New(svc, repo, log)
}

func newHTTPServer(api *adminhttp.Server) *http.Server {
	port := envOr("DISPUTE_HTTP_PORT", "8080")
	mux := http.NewServeMux()
	api.Mount(mux)
	return &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("dispute-service listening", zap.String("addr", srv.Addr))
			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Error("listen failed", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	})
}

// startExpireOverdueCron 每 10 min 跑 ExpireOverdue.
func startExpireOverdueCron(lc fx.Lifecycle, svc *workflow.Service, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
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
							log.Info("dispute auto-expired", zap.Int("count", n))
						} else if err != nil {
							log.Warn("ExpireOverdue failed", zap.Error(err))
						}
						c()
					}
				}
			}()
			log.Info("dispute expire-overdue cron started")
			return nil
		},
		OnStop: func(_ context.Context) error { cancel(); return nil },
	})
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

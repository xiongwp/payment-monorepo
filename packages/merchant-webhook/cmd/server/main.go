// merchant-webhook 入口 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
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

	"reconcile-system/packages/merchant-webhook/internal/adminhttp"
	"reconcile-system/packages/merchant-webhook/internal/dispatcher"
	"reconcile-system/packages/merchant-webhook/internal/repository"
)

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newRepo,
			newDispatcher,
			newAdminAPI,
			newHTTPServer,
		),
		fx.Invoke(
			startHTTPServer,
			startDispatchWorker,
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

func newDispatcher(repo *repository.MemoryRepo, log *zap.Logger) *dispatcher.Dispatcher {
	disp := dispatcher.New(repo, "2026-05-01", log)
	// 接 attempt 历史 (商户能看每次推送结果, DLQ 时排查用)
	disp.SetAttemptRecorder(repo.AppendAttempt)
	return disp
}

func newAdminAPI(repo *repository.MemoryRepo, disp *dispatcher.Dispatcher, log *zap.Logger) *adminhttp.Server {
	return adminhttp.New(repo, disp, log)
}

func newHTTPServer(api *adminhttp.Server) *http.Server {
	port := envOr("MERCHANT_WEBHOOK_HTTP_PORT", "8080")
	mux := http.NewServeMux()
	api.Mount(mux)
	api.MountDLQ(mux, envOr("MERCHANT_WEBHOOK_ADMIN_TOKEN", ""))
	return &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("merchant-webhook listening", zap.String("addr", srv.Addr))
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

// startDispatchWorker 每秒扫 ready_to_deliver 串行投递.
func startDispatchWorker(lc fx.Lifecycle, disp *dispatcher.Dispatcher, log *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
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
							log.Info("webhook batch delivered", zap.Int("count", n))
						} else if err != nil {
							log.Warn("DeliverPending failed", zap.Error(err))
						}
						c()
					}
				}
			}()
			log.Info("merchant-webhook dispatch worker started")
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

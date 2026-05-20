// approval-service 入口 — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
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

	"reconcile-system/packages/approval-service/internal/httpapi"
)

func main() {
	fx.New(
		fx.Provide(newLogger, newAPI, newHTTPServer),
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

func newAPI(log *zap.Logger) *httpapi.Server {
	return httpapi.New(log)
}

func newHTTPServer(api *httpapi.Server) *http.Server {
	addr := os.Getenv("APPROVAL_ADDR")
	if addr == "" {
		addr = ":8092"
	}
	return &http.Server{Addr: addr, Handler: api.Routes(), ReadHeaderTimeout: 5 * time.Second}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("approval-service listening", zap.String("addr", srv.Addr))
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

// approval-service 入口.
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/approval-service/internal/httpapi"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	srv := httpapi.New(log)

	addr := os.Getenv("APPROVAL_ADDR")
	if addr == "" {
		addr = ":8092"
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv.Routes(), ReadHeaderTimeout: 5 * time.Second}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		log.Info("approval-service listening", zap.String("addr", addr))
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server", zap.Error(err))
		}
	}()
	<-ctx.Done()
	shCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	_ = httpSrv.Shutdown(shCtx)
}

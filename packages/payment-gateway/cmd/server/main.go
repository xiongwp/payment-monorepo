// payment-gateway server 入口。
//
// 启动:
//   PAYMENT_GATEWAY_HTTP_PORT=8080 ./payment-gateway

package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"reconcile-system/packages/payment-gateway/internal/adminhttp"
	"reconcile-system/packages/payment-gateway/internal/repository"
	"reconcile-system/packages/payment-gateway/internal/routing"
	"reconcile-system/packages/payment-gateway/internal/tokenize"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := envOr("PAYMENT_GATEWAY_HTTP_PORT", "8080")

	// tokenize
	vault := repository.NewMemoryVault()
	kms := repository.NopKMS{Key: 0x42}
	tok := tokenize.New(kms, vault)

	// router with hardcoded sample channels (生产从 config-center 拉)
	channels := []routing.Channel{
		{ID: "visa-stripe", Name: "Visa via Stripe", Products: []string{"card_charge"},
			SupportedBINs: []string{"400000-499999"}, SupportedCcy: []string{"USD", "PHP", "SGD"},
			Regions: []string{"PH", "SG", "US"}, FeeBPS: 290, Priority: 80},
		{ID: "mc-adyen", Name: "Mastercard via Adyen", Products: []string{"card_charge"},
			SupportedBINs: []string{"510000-559999"}, SupportedCcy: []string{"USD", "PHP", "SGD"},
			Regions: []string{"PH", "SG"}, FeeBPS: 270, Priority: 70},
		{ID: "gcash-direct", Name: "GCash direct", Products: []string{"wallet"},
			SupportedCcy: []string{"PHP"}, Regions: []string{"PH"}, FeeBPS: 150, Priority: 90},
		{ID: "bank-ph", Name: "PH bank transfer", Products: []string{"bank_transfer"},
			SupportedCcy: []string{"PHP"}, Regions: []string{"PH"}, FeeBPS: 50,
			MinAmountMinor: 100000, Priority: 50},
	}
	router := routing.New(channels)

	mux := http.NewServeMux()
	api := adminhttp.New(tok, router, logger)
	api.Mount(mux)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger.Info("payment-gateway listening", zap.String("addr", srv.Addr))
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

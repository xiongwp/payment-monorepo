// tokenization-vault cmd/server — 入口.
//
// Env:
//   VAULT_ADDR             默认 ":8089"
//   VAULT_ADMIN_TOKEN      /admin/* 鉴权 token
//   VAULT_TOKEN_ENV        "live" | "test", 嵌入 token 前缀
//   VAULT_DEK_HEX          32-byte AES key, 64 hex 字符. 生产由 KMS 解 envelope.
//   VAULT_VTS_SIGNING_KEY  VTS provider 签名 key (dev stub 用)
//   VAULT_MDES_SIGNING_KEY MDES provider 签名 key

package main

import (
	"context"
	"encoding/hex"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"reconcile-system/packages/tokenization-vault/internal/adminhttp"
	"reconcile-system/packages/tokenization-vault/internal/audit"
	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/metrics"
	"reconcile-system/packages/tokenization-vault/internal/providers"
	"reconcile-system/packages/tokenization-vault/internal/providers/inhouse"
	"reconcile-system/packages/tokenization-vault/internal/providers/mdes"
	"reconcile-system/packages/tokenization-vault/internal/providers/vts"
	"reconcile-system/packages/tokenization-vault/internal/store"
	"reconcile-system/packages/tokenization-vault/internal/vault"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync()

	// DEK: prod 由 KMS envelope; dev 取 env (64hex) or 用 deterministic dev key
	dek := loadDEK(log)

	mem := store.NewMemStore()

	provs := map[domain.TokenProvider]providers.Provider{
		domain.ProviderVTS:     vts.New([]byte(os.Getenv("VAULT_VTS_SIGNING_KEY"))),
		domain.ProviderMDES:    mdes.New([]byte(os.Getenv("VAULT_MDES_SIGNING_KEY"))),
		domain.ProviderInhouse: inhouse.New(),
	}

	v := &vault.Vault{
		Store:     mem,
		Providers: provs,
		DEK:       dek,
		TokenEnv:  getenv("VAULT_TOKEN_ENV", "test"),
	}

	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)

	auditSink := audit.NewHTTPSink(audit.HTTPConfig{
		BaseURL: os.Getenv("AUDITLOG_URL"),
		Token:   os.Getenv("AUDITLOG_TOKEN"),
		Service: "tokenization-vault",
	}, log)
	defer auditSink.Stop()

	srv := &adminhttp.Server{
		Vault:      v,
		Store:      mem,
		Audit:      auditSink,
		AdminToken: os.Getenv("VAULT_ADMIN_TOKEN"),
		Log:        log,
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())

	addr := getenv("VAULT_ADDR", ":8089")
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("tokenization-vault listening", zap.String("addr", addr))
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

func loadDEK(log *zap.Logger) []byte {
	h := os.Getenv("VAULT_DEK_HEX")
	if h == "" {
		// 32 字节 "vault-dev-dek-do-NOT-use-in-prod" — 仅 dev
		log.Warn("VAULT_DEK_HEX not set, using deterministic dev key (DO NOT USE IN PROD)")
		return []byte("vault-dev-dek-do-NOT-use-in-prod")
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		log.Fatal("VAULT_DEK_HEX must be 64 hex chars (32 bytes)", zap.Error(err))
	}
	return b
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

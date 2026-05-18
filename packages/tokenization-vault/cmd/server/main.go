// tokenization-vault cmd/server — uber/fx 装配, 跟 order-core / accounting-system 同款风格.
//
// Env:
//
//	VAULT_ADDR             默认 ":8089"
//	VAULT_ADMIN_TOKEN      /admin/* 鉴权 token
//	VAULT_TOKEN_ENV        "live" | "test", 嵌入 token 前缀
//	VAULT_DEK_HEX          32-byte AES key, 64 hex 字符. 生产由 KMS 解 envelope.
//	VAULT_VTS_SIGNING_KEY  VTS provider 签名 key (dev stub 用)
//	VAULT_MDES_SIGNING_KEY MDES provider 签名 key
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
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

// dek 是 32-byte AES key, 单独类型避免 fx []byte 撞.
type dekKey []byte

func main() {
	fx.New(
		fx.Provide(
			newLogger,
			newDEK,
			newStore,
			newProviders,
			newVault,
			newPromRegistry,
			newAuditSink,
			newAdminServer,
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

// newDEK prod 由 KMS envelope; dev 取 env (64hex) or deterministic dev key.
func newDEK(log *zap.Logger) dekKey {
	h := os.Getenv("VAULT_DEK_HEX")
	if h == "" {
		log.Warn("VAULT_DEK_HEX not set, using deterministic dev key (DO NOT USE IN PROD)")
		return dekKey("vault-dev-dek-do-NOT-use-in-prod")
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 32 {
		log.Fatal("VAULT_DEK_HEX must be 64 hex chars (32 bytes)", zap.Error(err))
	}
	return dekKey(b)
}

func newStore() *store.MemStore { return store.NewMemStore() }

func newProviders() map[domain.TokenProvider]providers.Provider {
	return map[domain.TokenProvider]providers.Provider{
		domain.ProviderVTS:     vts.New([]byte(os.Getenv("VAULT_VTS_SIGNING_KEY"))),
		domain.ProviderMDES:    mdes.New([]byte(os.Getenv("VAULT_MDES_SIGNING_KEY"))),
		domain.ProviderInhouse: inhouse.New(),
	}
}

func newVault(s *store.MemStore, provs map[domain.TokenProvider]providers.Provider, dek dekKey) *vault.Vault {
	return &vault.Vault{
		Store:     s,
		Providers: provs,
		DEK:       []byte(dek),
		TokenEnv:  getenv("VAULT_TOKEN_ENV", "test"),
	}
}

func newPromRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	metrics.MustRegister(reg)
	return reg
}

func newAuditSink(lc fx.Lifecycle, log *zap.Logger) *audit.HTTPSink {
	sink := audit.NewHTTPSink(audit.HTTPConfig{
		BaseURL: os.Getenv("AUDITLOG_URL"),
		Token:   os.Getenv("AUDITLOG_TOKEN"),
		Service: "tokenization-vault",
	}, log)
	lc.Append(fx.Hook{OnStop: func(_ context.Context) error { sink.Stop(); return nil }})
	return sink
}

func newAdminServer(v *vault.Vault, s *store.MemStore, sink *audit.HTTPSink, log *zap.Logger) *adminhttp.Server {
	return &adminhttp.Server{
		Vault:      v,
		Store:      s,
		Audit:      sink,
		AdminToken: os.Getenv("VAULT_ADMIN_TOKEN"),
		Log:        log,
	}
}

func newHTTPServer(srv *adminhttp.Server, reg *prometheus.Registry) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.Handle("/", srv.Routes())
	return &http.Server{
		Addr:              getenv("VAULT_ADDR", ":8089"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func startHTTPServer(lc fx.Lifecycle, srv *http.Server, log *zap.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			log.Info("tokenization-vault listening", zap.String("addr", srv.Addr))
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
			return srv.Shutdown(shCtx)
		},
	})
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

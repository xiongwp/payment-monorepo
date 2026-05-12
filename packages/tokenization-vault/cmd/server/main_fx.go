// main_fx.go — tokenization-vault uber/fx 版.

//go:build fx

package main

import (
	"encoding/hex"
	"os"

	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"reconcile-system/packages/tokenization-vault/internal/adminhttp"
	"reconcile-system/packages/tokenization-vault/internal/audit"
	"reconcile-system/packages/tokenization-vault/internal/domain"
	"reconcile-system/packages/tokenization-vault/internal/providers"
	"reconcile-system/packages/tokenization-vault/internal/providers/inhouse"
	"reconcile-system/packages/tokenization-vault/internal/providers/mdes"
	"reconcile-system/packages/tokenization-vault/internal/providers/vts"
	"reconcile-system/packages/tokenization-vault/internal/repo"
	"reconcile-system/packages/tokenization-vault/internal/store"
	"reconcile-system/packages/tokenization-vault/internal/vault"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "tokenization-vault",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		Models: []any{
			&domain.InternalTokenGormModel{},
			&domain.EncryptedPANGormModel{},
		},

		AppModules: []fx.Option{
			// DEK — KMS envelope (prod) / env-supplied (dev)
			fx.Provide(func(log *zap.Logger) []byte {
				h := os.Getenv("VAULT_DEK_HEX")
				if h == "" {
					log.Warn("VAULT_DEK_HEX not set, using deterministic dev key")
					return []byte("vault-dev-dek-do-NOT-use-in-prod")
				}
				b, err := hex.DecodeString(h)
				if err != nil || len(b) != 32 {
					log.Fatal("bad VAULT_DEK_HEX")
				}
				return b
			}),

			// Store: GORM (有 DB) / Memory (无 DB)
			fx.Provide(func(app *scaffold.App, log *zap.Logger) store.Store {
				if app.DB != nil {
					return repo.NewGormRepo(app.DB.GORM(), log)
				}
				log.Info("using MemStore")
				return store.NewMemStore()
			}),

			// 3 providers
			fx.Provide(func() map[domain.TokenProvider]providers.Provider {
				return map[domain.TokenProvider]providers.Provider{
					domain.ProviderVTS:     vts.New([]byte(os.Getenv("VAULT_VTS_SIGNING_KEY"))),
					domain.ProviderMDES:    mdes.New([]byte(os.Getenv("VAULT_MDES_SIGNING_KEY"))),
					domain.ProviderInhouse: inhouse.New(),
				}
			}),

			// Vault
			fx.Provide(func(s store.Store, provs map[domain.TokenProvider]providers.Provider, dek []byte) *vault.Vault {
				return &vault.Vault{
					Store:     s,
					Providers: provs,
					DEK:       dek,
					TokenEnv:  os.Getenv("VAULT_TOKEN_ENV"),
				}
			}),

			// audit sink
			fx.Provide(func(cfg *scaffold.Config, log *zap.Logger) audit.Sink {
				return audit.NewHTTPSink(audit.HTTPConfig{
					BaseURL: cfg.Audit.BaseURL,
					Token:   cfg.Audit.Token,
					Service: cfg.ServiceName,
				}, log)
			}),

			// adminhttp.Server
			fx.Provide(func(v *vault.Vault, s store.Store, ad audit.Sink, cfg *scaffold.Config, log *zap.Logger) *adminhttp.Server {
				return &adminhttp.Server{
					Vault: v, Store: s, Audit: ad,
					AdminToken: cfg.AdminToken,
					Log:        log,
				}
			}),

			// Mount routes (TODO: 桥 stdlib → fiber via fasthttpadaptor)
			fx.Invoke(func(app *scaffold.App, srv *adminhttp.Server) {
				_ = srv
			}),
		},
	})
}

// main_fx.go — aml-screening uber/fx 版 (reference).
//
// build tag "fx" 启用; 跟 main.go 二选一.
//
// 这只是骨架 — 实际迁移时需要把现有 stdlib net/http handler 桥到 fiber.
// 桥方案:
//   import "github.com/valyala/fasthttp/fasthttpadaptor"
//   app.Fiber.All("/admin/*", func(c *fiber.Ctx) error {
//       fasthttpadaptor.NewFastHTTPHandler(srv.Routes())(c.Context())
//       return nil
//   })

//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"reconcile-system/packages/aml-screening/internal/adminhttp"
	"reconcile-system/packages/aml-screening/internal/audit"
	"reconcile-system/packages/aml-screening/internal/screening"
	"reconcile-system/packages/aml-screening/internal/sources"
	"reconcile-system/packages/aml-screening/internal/store"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "aml-screening",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",

		AppModules: []fx.Option{
			// Store: MemStore (无 DB) / MySQL (有 DB)
			fx.Provide(func(app *scaffold.App, log *zap.Logger) store.Store {
				if app.DB != nil {
					return store.NewMySQLStore(app.DB.SQL())
				}
				log.Info("using MemStore (no DB DSN)")
				return store.NewMemStore()
			}),

			fx.Provide(screening.DefaultConfig),

			fx.Provide(func(cfg *scaffold.Config, log *zap.Logger) audit.Sink {
				return audit.NewHTTPSink(audit.HTTPConfig{
					BaseURL: cfg.Audit.BaseURL,
					Token:   cfg.Audit.Token,
					Service: cfg.ServiceName,
				}, log)
			}),

			// dev seed
			fx.Invoke(func(s store.Store, log *zap.Logger) error {
				if err := sources.SeedDev(s); err != nil {
					log.Warn("seed dev failed", zap.Error(err))
				}
				return nil
			}),

			fx.Provide(func(s store.Store, cfg *scaffold.Config, ad audit.Sink, log *zap.Logger, scrCfg screening.Config) *adminhttp.Server {
				return &adminhttp.Server{
					Store:      s,
					Cfg:        scrCfg,
					Audit:      ad,
					AdminToken: cfg.AdminToken,
					Log:        log,
				}
			}),

			// 路由挂载 — 真生产用 fasthttpadaptor 桥 stdlib http.Handler 到 fiber.
			fx.Invoke(func(app *scaffold.App, srv *adminhttp.Server) {
				// TODO: 用 fasthttpadaptor.NewFastHTTPHandler(srv.Routes()) 桥到 app.Fiber
				_ = srv
			}),
		},
	})
}

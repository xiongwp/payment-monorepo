// main_fx.go — aml-screening uber/fx 版.

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

//go:embed ../../openapi.yaml
// var openapiSpec []byte

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "aml-screening",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",

		AppModules: []fx.Option{
			// Store: memory (dev) / GORM (prod) — 跟 cfg.DB.DSN 走
			fx.Provide(func(app *scaffold.App, log *zap.Logger) store.Store {
				if app.DB != nil {
					return store.NewMySQLStore(app.DB.SQL())
				}
				return store.NewMemStore()
			}),

			// 配置
			fx.Provide(func() screening.Config { return screening.DefaultConfig() }),

			// audit sink
			fx.Provide(func(cfg *scaffold.Config, log *zap.Logger) audit.Sink {
				return audit.NewHTTPSink(audit.HTTPConfig{
					BaseURL: cfg.Audit.BaseURL,
					Token:   cfg.Audit.Token,
					Service: cfg.ServiceName,
				}, log)
			}),

			// dev seed (test data)
			fx.Invoke(func(s store.Store, log *zap.Logger) error {
				return sources.SeedDev(s)
			}),

			// admin Server
			fx.Provide(func(s store.Store, cfg *scaffold.Config, audSink audit.Sink, log *zap.Logger, scrCfg screening.Config) *adminhttp.Server {
				return &adminhttp.Server{
					Store:      s,
					Cfg:        scrCfg,
					Audit:      audSink,
					AdminToken: cfg.AdminToken,
					Log:        log,
				}
			}),

			// 注册路由 — handlers 全装到 fiber
			fx.Invoke(func(app *scaffold.App, srv *adminhttp.Server) {
				// Server 提供 http.Handler; 桥到 fiber 用 adaptor
				app.Fiber.All("/v1/*", func(c *fiber.Ctx) error {
					// 简化: 真接入需要 fasthttpadaptor 转 fiber→http.Handler
					return c.SendStatus(501)
				})
				app.Fiber.All("/admin/*", srv.Routes().ServeHTTP) // pseudo
				_ = srv
			}),
		},
	})
}

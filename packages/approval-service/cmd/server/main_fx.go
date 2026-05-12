// main_fx.go — approval-service uber/fx 版.
//
// 重构亮点: 用 GormStore 替代 in-memory Server.actions map.

//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"reconcile-system/packages/approval-service/internal/httpapi"
)

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "approval-service",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		Models: []any{
			&httpapi.ActionGormModel{},
		},

		AppModules: []fx.Option{
			// GormStore 替代原 in-memory
			fx.Provide(func(app *scaffold.App, log *zap.Logger) *httpapi.GormStore {
				if app.DB == nil {
					log.Fatal("approval-service requires DB DSN (no in-memory fallback)")
				}
				return httpapi.NewGormStore(app.DB.GORM(), log)
			}),

			// Server 持 GormStore + log
			fx.Provide(func(s *httpapi.GormStore, log *zap.Logger) *httpapi.Server {
				return httpapi.NewWithStore(s, log)
			}),

			// 路由
			fx.Invoke(func(app *scaffold.App, srv *httpapi.Server) {
				_ = srv
				// TODO: bridge srv.Routes() → fiber
			}),
		},
	})
}

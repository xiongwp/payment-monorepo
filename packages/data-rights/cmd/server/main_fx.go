// main_fx.go — uber/fx 版本.
//
// 跟 main_v2.go (scaffold.Run) 对比:
//   - main_v2: 业务 deps 手动 new (闭包内)
//   - main_fx: 业务 deps 走 fx.Provide, 互相 wire 自动, mock 测试时 fx.Decorate 改一行
//
// 跑这个版本: build tag "fx" 启用; main_v2 build tag "scaffold"; 老 main.go 无 tag 默认.

//go:build fx

package main

import (
	"github.com/xiongwp/payment-util/scaffold"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"reconcile-system/packages/data-rights/internal/audit"
	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/handler"
	"reconcile-system/packages/data-rights/internal/orchestrator"
	"reconcile-system/packages/data-rights/internal/repo"
	"reconcile-system/packages/data-rights/internal/service"
	"reconcile-system/packages/data-rights/internal/store"
)

// _ "embed"; openapi //go:embed openapi.yaml
// var openapiSpec []byte

func main() {
	scaffold.RunFx(scaffold.FxOpts{
		ServiceName:  "data-rights",
		ConfigPath:   "config/config.yaml",
		MigrationDir: "migrations",
		Models: []any{
			&domain.RequestGormModel{},
			&domain.ServiceStatusGormModel{},
		},
		// OpenAPISpec: openapiSpec,    // 启用后 /docs 自动有交互文档
		OpenAPIInfo: scaffold.OpenAPIInfo{Title: "Data Rights API", Version: "1.0.0"},

		// 业务 fx Modules — 自动 wire 依赖图
		AppModules: []fx.Option{
			// repo
			fx.Provide(func(app *scaffold.App, log *zap.Logger) store.Store {
				return repo.NewGormRepo(app.DB.GORM(), log)
			}),

			// audit sink (拿 cfg.Audit 自动)
			fx.Provide(func(cfg *scaffold.Config, log *zap.Logger) audit.Sink {
				return audit.NewHTTPSink(audit.HTTPConfig{
					BaseURL: cfg.Audit.BaseURL,
					Token:   cfg.Audit.Token,
					Service: cfg.ServiceName,
				}, log)
			}),

			// orchestrator (跨服务 fan-out 客户端)
			fx.Provide(func(s store.Store, log *zap.Logger) *orchestrator.Orchestrator {
				return orchestrator.New(s, orchestrator.DefaultRegistry(), log)
			}),

			// 业务 service
			fx.Provide(service.New),

			// handler — 拿 *service.Service + log
			fx.Provide(handler.New),

			// 注册路由 — 业务侧 fx.Invoke
			fx.Invoke(func(app *scaffold.App, h *handler.Handler) {
				h.Mount(app)
			}),
		},
	})
}

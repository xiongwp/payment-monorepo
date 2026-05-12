// main_v2.go — 用 scaffold 的版本 (参考).
//
// 跟原 main.go 对比:
//   原版: 100+ 行 (zap / store / orch / mux / handler / signal / shutdown 全手写)
//   新版: 30 行 (scaffold 自动)
//
// build tag 隔开避免 duplicate main; 上线时把原 main.go 删, 这个改回 main.go.

//go:build scaffold

package main

import (
	"github.com/xiongwp/payment-util/scaffold"

	"reconcile-system/packages/data-rights/internal/audit"
	"reconcile-system/packages/data-rights/internal/domain"
	"reconcile-system/packages/data-rights/internal/handler"
	"reconcile-system/packages/data-rights/internal/orchestrator"
	"reconcile-system/packages/data-rights/internal/repo"
	"reconcile-system/packages/data-rights/internal/service"
)

func main() {
	scaffold.Run(scaffold.Opts{
		ServiceName: "data-rights",
		ConfigPath:  "config/config.yaml",
		Models: []interface{}{
			&domain.RequestGormModel{},
			&domain.ServiceStatusGormModel{},
		},
		RegisterRoutes: func(app *scaffold.App, cfg *scaffold.Config) error {
			// 装配业务层
			gormRepo := repo.NewGormRepo(app.DB.GORM(), app.Log)

			// audit sink (HTTPSink if AUDITLOG_URL else LogSink)
			auditSink := audit.NewHTTPSink(audit.HTTPConfig{
				BaseURL: cfg.Audit.BaseURL,
				Token:   cfg.Audit.Token,
				Service: cfg.ServiceName,
			}, app.Log)

			// orchestrator (registry from config-center or default)
			orch := orchestrator.New(gormRepo, orchestrator.DefaultRegistry(), app.Log)

			// service
			svc := service.New(gormRepo, orch, auditSink, app.Log)

			// handler — 装载路由到 fiber
			h := handler.New(svc, app.Log)
			h.Mount(app)
			return nil
		},
	})
}

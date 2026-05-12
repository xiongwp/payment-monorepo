// Package scaffold — 全平台服务统一脚手架.
//
// 目标:
//   1. 所有 service 同样的代码结构 (新人 onboarding 半天上手所有服务)
//   2. 用 GORM 替代 raw database/sql, 减少 ~40% 数据访问代码
//   3. 用 Fiber 替代 stdlib net/http, 中间件链统一
//   4. 内置: oauth2 bearer / metrics / RFC7807 / req-id / ratelimit / recover / logger
//
// canonical 目录:
//
//   <service>/
//     cmd/server/main.go      — main: 5-7 行调 scaffold.Run
//     internal/
//       domain/               — pure types, 无依赖
//       repo/                 — GORM repository, 实现 ports
//       service/              — 业务编排
//       handler/              — fiber handler (路由注册)
//       config/config.go      — viper struct (scaffold 加载)
//     migrations/             — gorm.AutoMigrate model 列表
//     Dockerfile              — 统一 multi-stage
//     docker-compose.yml
//     deploy/k8s/
//     README.md
//     CLAUDE.md
//
// 用法 (新服务 main.go 整体长这样):
//
//   func main() {
//       scaffold.Run(scaffold.Opts{
//           ServiceName: "data-rights",
//           Port: 8091,
//           ConfigPath: "config/config.yaml",
//           Models: []interface{}{
//               &domain.Request{},
//               &domain.ServiceStatus{},
//           },
//           RegisterRoutes: handler.Register,
//           // optional:
//           BeforeStart: func(app *scaffold.App) error { /*...*/ return nil },
//           AfterStop:   func() { /*...*/ },
//       })
//   }
//
// scaffold.Run 自动:
//   1. 加载 config (yaml + env override)
//   2. zap logger (json prod / human dev)
//   3. GORM open + AutoMigrate Models
//   4. Fiber app + 全套中间件
//   5. 注册业务 routes
//   6. /healthz + /metrics 自动有
//   7. signal handling + graceful shutdown
//   8. /admin/* 自动加 admin token 守护
//
// 业务 service handler 只关心业务; 不需要再写 zap / db / health / metrics / shutdown 重复代码.

package scaffold

# 服务迁移指南: 接入统一脚手架 (scaffold + GORM + Fiber + uber/fx)

新脚手架 `payment-util/scaffold` 提供:
- **GORM** 替代 raw `database/sql` + 手写 JSON marshal/unmarshal
- **Fiber** 替代 stdlib `net/http` + 手装路由
- **标准中间件链**: recover / req-id / logger / metrics / cors / ratelimit / oauth2 bearer
- **统一错误**: RFC 7807 problem+json
- **生命周期管理**: config 加载 / db 初始化 / graceful shutdown / signal handling

收益:
- 新服务 ~30 行 main.go (原 ~100 行)
- 所有服务一样的结构 — 新人 1 天上手任一服务
- middleware 升级一处 ⇒ 全平台生效 (例: 加 OTel trace propagation)

---

## Canonical 目录结构

每个服务最终应该长这样:

```
<service>/
  cmd/server/
    main.go                — scaffold.Run(opts), ~30 行
  internal/
    domain/                — pure types + GORM models (gorm_models.go)
    repo/                  — GORM repository (实现 Store interface)
    service/               — 业务编排 (handler → service → repo)
    handler/               — Fiber handler 注册路由
    audit/                 — 自己的 audit event types + HTTPSink (从 scaffold 拿)
    metrics/               — service-specific prometheus
    (orchestrator/外部 client) — 视服务而定
  config/
    config.yaml            — scaffold 解析
    config.docker.yaml
  migrations/              — schema (生产用; dev 走 AutoMigrate)
  Dockerfile
  docker-compose.yml
  deploy/k8s/<svc>.yaml
  README.md
  CLAUDE.md
```

## main.go 模板

```go
package main

import (
    "github.com/xiongwp/payment-util/scaffold"

    "myservice/internal/domain"
    "myservice/internal/handler"
    "myservice/internal/repo"
    "myservice/internal/service"
)

func main() {
    scaffold.Run(scaffold.Opts{
        ServiceName: "my-service",
        ConfigPath:  "config/config.yaml",
        Models: []interface{}{
            &domain.FooGormModel{},
            &domain.BarGormModel{},
        },
        RegisterRoutes: func(app *scaffold.App, cfg *scaffold.Config) error {
            r := repo.NewGormRepo(app.DB.GORM(), app.Log)
            svc := service.New(r, app.Log)
            h := handler.New(svc, app.Log)
            h.Mount(app)
            return nil
        },
    })
}
```

`scaffold.Run` 自动:
- 加载 yaml + env config
- 构造 zap logger
- 打开 GORM DB + 连接池 + AutoMigrate
- 创建 Fiber app + 全套中间件
- 公共路由: `/healthz` `/readyz` `/metrics`
- 业务 routes via `RegisterRoutes`
- SIGTERM 后 graceful shutdown

## 配置约定

`config/config.yaml`:

```yaml
service_name: my-service
addr: ":8090"
log_level: "info"
log_dev: false
admin_token: ""           # env override: MY_SERVICE_ADMIN_TOKEN
shutdown_timeout: "10s"

db:
  dsn: ""                 # env: MY_SERVICE_DB_DSN
  max_open_conns: 30
  max_idle_conns: 10
  conn_max_lifetime: "30m"
  slow_threshold: "200ms"
  auto_migrate: false     # dev=true, prod=false

oauth2:
  introspect_url: "http://oauth2-server:8087/oauth2/introspect"
  required: true

audit:
  base_url: "http://audit-log:8087"
  token: ""

rate:
  enabled: true
  global_rps: 1000
  per_key_rps: 100
```

Env override 优先级最高. 命名: `{SERVICE_NAME}_{FIELD}_{SUBFIELD}` (大写, `-`→`_`).
例: `MY_SERVICE_DB_DSN`, `DATA_RIGHTS_ADDR`.

## 迁移步骤 (现有服务)

### 1) 加 scaffold 依赖

```bash
cd <service>
go get github.com/xiongwp/payment-util/scaffold
```

### 2) 加 domain GORM models

为每个表加 `domain.FooGormModel` + `TableName()` + `FromDomainFoo` / `ToDomain` 双向转换. 参考 `data-rights/internal/domain/gorm_models.go`.

### 3) 写 GORM repo 实现现有 Store interface

复用现有 `store.Store` interface — repo 只换实现 (MemStore → GormRepo). 业务层 `service` 不动. 参考 `data-rights/internal/repo/gorm_repo.go`.

### 4) 提取 service 层 (如果原代码业务逻辑在 handler 里)

按 canonical 拆: handler 只解请求 + 调 service; service 持有 repo + audit + 外部 client + 业务编排.

### 5) 替换 handler — 从 net/http 改到 Fiber

```go
// 之前 (net/http):
mux.HandleFunc("/v1/foo", func(w http.ResponseWriter, r *http.Request) {
    var body MyBody
    json.NewDecoder(r.Body).Decode(&body)
    // ...
    json.NewEncoder(w).Encode(out)
})

// 之后 (Fiber):
func (h *Handler) Mount(app *scaffold.App) {
    app.Fiber.Post("/v1/foo", func(c *fiber.Ctx) error {
        var body MyBody
        if err := c.BodyParser(&body); err != nil {
            return scaffold.ErrorWith(c, 400, "bad_json", err.Error())
        }
        out, err := h.svc.DoFoo(c.Context(), body)
        if err != nil {
            return scaffold.ErrorWith(c, 500, "internal_error", err.Error())
        }
        return c.JSON(out)
    })
}
```

### 6) 把 main.go 改成 30 行 scaffold.Run 调用

参考 `data-rights/cmd/server/main_v2.go`.

### 7) 跑测试 / 烟雾验证

```bash
make test               # 单测应该全过 (业务逻辑没动, 只换数据访问层)
make image
docker compose up -d
bash test/smoke.sh
```

## 渐进迁移策略

**不要一次全迁** — 风险大. 按以下顺序:

| 阶段 | 时机 | 服务 |
|---|---|---|
| Phase 0 | DONE | scaffold lib + data-rights 范例 |
| Phase 1 | Sprint 1 | 新服务 from-scratch 用 scaffold (default) |
| Phase 2 | Sprint 2-3 | 4 个 P0 新服务 (aml / vault / tax) — 没历史包袱 |
| Phase 3 | Sprint 4-6 | 8 个新业务服务 (billing / clearing / dispute / refund / webhook / kyc / wallet / approval) |
| Phase 4 | Sprint 7+ | 老核心 (payment-core / order-core / payment-channel / user-merchant-core / accounting-system) — 最危险, 灰度切 5% / 25% / 100% |
| 不迁 | 永久 | reconplatform (有独立架构 / cytoscape SPA / Starlark engine, 跟 scaffold 风格不匹配) |

## 灰度切流策略 (Phase 4)

```
┌──────────────────────────────────┐
│  api-gateway / payment-mw         │
│   按 X-Canary header 路由         │
└──────────────────────────────────┘
       │                  │
       ▼                  ▼
   svc-old:9090       svc-new:9091  (scaffold 版)
   (95% 流量)         (5% 流量)
```

监控:
- error rate 不能上升 > 0.1%
- p99 latency 不能上升 > 50ms
- DB conn pool / GC 表现稳定

24h 稳定后切 25%; 再 24h 切 100%; 再 1 周后下线旧版本.

## 不要做的

- ❌ 不要混用 stdlib net/http 和 Fiber 在同一服务里 (中间件链分裂)
- ❌ 不要在新服务用 raw `database/sql` (GORM AutoMigrate + WithContext 比手写 SQL 强很多)
- ❌ 不要在 service 层 import fiber (隔离业务 from web framework)
- ❌ 不要在 handler 层写业务逻辑 (handler 只解请求 + 调 service)
- ❌ 不要绕过 scaffold.ErrorWith — 错误格式必须统一 (RFC 7807)

## FAQ

**Q: GORM 性能不如 raw SQL?**
A: AutoMigrate / 复杂 join 慢; 简单 CRUD 跟 raw 几乎一样 (PrepareStmt 缓存). 真热路径用 `db.GORM().Raw("...")` 绕过 ORM.

**Q: Fiber 比 gin 好?**
A: Fiber 基于 fasthttp, 比 gin 快 30-40%. 但 ctx 不是 stdlib 的 `*http.Request`, 第三方库不能直接用. 大部分场景 OK; 需要 stdlib 的换 chi / gin.

**Q: 不迁 reconplatform 吗?**
A: reconplatform 跟其他服务架构差很多 (Starlark engine + cytoscape SPA + 独立 admin), 迁过去得罪太多代码; 性价比低. 保留独立架构.

**Q: 老服务有 grpc, scaffold 只支持 HTTP?**
A: scaffold 目前只装 fiber. gRPC server 自己另开 listener (跟 fiber 同 process). 真要统一可以扩 scaffold 加 grpc.Server 支持.

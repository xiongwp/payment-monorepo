# config-center 状态 & 功能矩阵

## v1.0（生产可跑）— 已完成

| # | v1.0 deliverable | 文件 |
| --- | --- | --- |
| 1 | repo GORM 实装（5 核心接口 + admin 查询 + transaction 事务保护） | `internal/repo/repo.go` |
| 2 | HTTP REST API（GET/PUT/Rollback） + SSE long-stream watch | `internal/server/http_api.go` |
| 3 | WebSocket / SSE / HTTP 三通道共享同一 service.WatchNamespace channel | 同上（SSE 已 wire；WS 留 hook） |
| 4 | service 层 admin DTO + AdminRepo 接口（admin handler 不直接 import repo） | `internal/service/admin.go` |
| 5 | fx main + assertProdSafety + etcd 自注册 + Prometheus | `cmd/server/{main,registry}.go` |
| 6 | Dockerfile + Makefile（含 `make proto`）+ docker-compose.yml + `config/config.yaml` | 见根目录 |
| 7 | proto 契约（v1.0 SDK 走 HTTP；proto 给后续 grpc handler wire 用） | `api/proto/configcenter/v1/configcenter.proto` |
| 8 | SDK HTTP rpcClient（SSE long-stream + 自动重连 + 双版本 cache） | `payment-util/configcenter/httprpc.go` |

**架构选择**：v1.0 SDK ↔ server 走 HTTP+SSE 而非 gRPC，避开沙箱无 protoc 的依赖；服务依然按其他服务的 fx + mTLS + etcd 模板组织。后续 `make proto` 跑过后可在不破坏 SDK 接口的前提下加 gRPC handler（rpcClient 已抽象为接口）。

## 全平台集成 — 11 个服务已接 SDK ✅

| 服务 | namespace | 集成深度 | 删的旧物 |
| --- | --- | --- | --- |
| accounting-system | accounting-system | **深度** — wrap SystemConfigService，9 caller 零改动 | DB 表 system_config + repo + reload 扇出 + 3 admin handler |
| api-gateway | api-gateway | **深度** — RateLimitHub + ApplyParams + OnChange 热更新 | rate_limit 静态字段 → 动态 SetLimit |
| user-merchant-core | user-merchant-core | SDK 注入（fxprovider） | yaml bootstrap 保留 |
| order-core | order-core | SDK 注入 | yaml bootstrap 保留 |
| payment-core | payment-core | SDK 注入 | yaml bootstrap 保留 |
| payment-channel | payment-channel | SDK 注入 | yaml bootstrap 保留 |
| risk-manage | risk-manage | SDK 注入 | yaml bootstrap 保留 |
| card-center | card-center | SDK 注入 | yaml bootstrap 保留 |
| card-payment | card-payment | SDK 注入 | yaml bootstrap 保留 |
| clearing-settlement | clearing-settlement | SDK 注入 | yaml bootstrap 保留 |
| kms-manage | kms-manage | SDK 注入 | yaml bootstrap 保留 |

**集成深度说明**：
- **深度** = 完成 yaml/DB 删除 + 业务代码改读 SDK + 热更新链路
- **SDK 注入** = main.go fx.Provide(configcenter.FxProvider("<ns>"))；docker-compose 注 endpoint + namespace；服务可注入 `*configcenter.Client`，业务读改 SDK 走渐进迁

后续每个服务的「业务代码改读 SDK」按 accounting-system 模板执行：替换 `cfg.X.Y` → `cli.GetXxx(ctx, "X.Y", def)`，或保留接口 wrap SDK（accounting-system 模式）。

**部署 / Docker**：
- `bash deploy-stack.sh up` 按依赖顺序拉起 14 stack；config-center 第一个，阻塞等 /healthz 200
- `bash deploy-stack.sh seed` 写入 7 条 accounting-system 历史 key
- 所有 11 服务 docker-compose 注入 `<PREFIX>_CONFIGCENTER_ENDPOINT` + `<PREFIX>_CONFIGCENTER_NAMESPACE`
- 共享外部网络 `payment-stack`，容器 DNS 走 `config-center` 即可

## 老系统集成 — 第 1 个：accounting-system（v2 已切）

| 删除 | 替换 |
| --- | --- |
| `packages/accounting-system/database/metadb/init/init.sql` 中的 `system_config` 表 + 7 条种子 INSERT | 注释保留为人工 seed 清单；config-center 启动后由运维 PUT 一次 |
| `packages/accounting-system/internal/repository/system_config_repository.go` | 文件物理删除 |
| `internal/service/system_config_service.go` 的 sync.RWMutex cache + admin POST /reload 扇出 | wrap `payment-util/configcenter.Client`，namespace="accounting-system"，key 1:1 保留 |
| adminhttp `/admin/config` `/admin/config/{key}` `/admin/reload/config` 真实 handler | 全部映射到 `handleSystemConfigDeprecated` 返 410 Gone + 引导走 config-center admin web |

**业务侧改动**：9 个 caller（accounting_service / outbox_worker / day_cut_scheduler 等）调 `systemConfigSvc.GetInt/GetString/GetBool/GetJSON` **零改动**——接口不变，底层换成 SDK 的纳秒级 atomic.Pointer 读。

**fx 装配**：
- 删 `repository.NewSystemConfigRepository`
- 加 `NewConfigCenterClient` provider（main.go 末尾）—— prod 环境 fail-fast；dev 环境 nil 兜底（service 层 nil-check 走 def）。

## 已完成（v0.1 skeleton）

| # | 功能 | 文件 |
| --- | --- | --- |
| 1 | 4 种发布策略 (FULL/CANARY/TARGETED/SCHEDULED) | `api/proto/configcenter/v1/configcenter.proto` |
| 2 | DB schema 5 张表（namespace / item / version / subscription / audit） | `database/metadb/init/init.sql` |
| 3 | service 层：PutConfig / Rollback / GetConfig / WatchNamespace + per-namespace fan-out hub | `internal/service/{service,hub}.go` |
| 4 | 客户端 SDK：本地 cache + atomic Bind + watch 自动重连 + 生效窗口校验 | `payment-util/configcenter/client.go` |
| 5 | 三种推送通道（gRPC stream / WebSocket / SSE long-poll） | proto + `internal/server/` (skeleton) |
| 6 | admin 管理页面 + 路由 | `internal/server/admin_html.go` |
| 7 | 配置项**新建** + 选择"应用到哪些系统"（多对多 subscriber） | admin `/items/new` + `config_subscription` 表 |
| 8 | 按服务名 / 按 key 名搜索 | admin `/ns/{ns}` + `/items?q=...` |
| 9 | **版本 diff** — LCS unified-diff 视图 | admin `/ns/{ns}/{key}/diff?from=N&to=M` |
| 10 | 版本列表 + 一键回滚 | admin `/ns/{ns}/{key}` 详情页 |
| 11 | 审计 log 时间序 | admin `/audit` |
| 12 | architecture & 迁移 plan 文档 | `README.md` |

## 健康 / 可扩展性（已加）

| 维度 | 实现 |
| --- | --- |
| **liveness** | HTTP `/healthz` — 进程没卡死即 OK，**不查依赖**（避免下游故障误杀 pod） |
| **readiness** | HTTP `/readyz` — DB ping + 注册的依赖 probe 全 OK + 不在 drain 才 200；任一 down → 503 + body 写具体哪个 down |
| **gRPC health** | `grpc.health.v1.Health/Check` — K8s gRPC probe / istio sidecar / SDK 客户端可用 |
| **drain 协调** | fx OnStop → BeginDrain → /readyz 立即 503 + grpc health NOT_SERVING；K8s 摘流量再 GracefulStop |
| **多副本 fan-out** | `internal/pusher/kafka_bridge.go` — admin write 既本地 hub.Publish 又发 Kafka topic `config-center.events`；其他副本消费回环 fan-out 给自己 watcher |
| **客户端 since_version resume** | Kafka 漏发 / 副本切换时，客户端用本地 maxVersion 重连后服务端补齐增量，最终一致 |
| **读副本路由** | DB GetActive / SinceVersion 路径可走 read replica（GetMetaRO 模式，跟其他服务对齐） |
| **SDK 启动期阻塞** | NewWithRPC 同步拉一次 snapshot；超时 + 0 数据 → 返 error 让 main fx fail-fast；防止服务带空 cache 上线 |

## SDK 类型化 K-V 读取（已加）

`payment-util/configcenter/typed.go` — 业务热路径不处理 raw string：

| API | 返回 | 备注 |
| --- | --- | --- |
| `GetString(ctx, key, def)` | `string` | 已在 client.go |
| `GetInt(ctx, key, def)` / `GetInt64` | `int` / `int64` | plain + json 都接受 |
| `GetFloat64(ctx, key, def)` | `float64` | 同上 |
| `GetBool(ctx, key, def)` | `bool` | 接受 true/1/yes/on 及 json bool |
| `GetDuration(ctx, key, def)` | `time.Duration` | 必须 `30s` 这种格式（不接受裸数字防单位歧义） |
| `GetStringList(ctx, key, def)` | `[]string` | json 数组 / CSV / 单值 三态兼容 |
| `GetIntList(ctx, key, def)` | `[]int` | 同上 |
| `GetMap(ctx, key, def)` | `map[string]string` | json 字典 |
| `GetJSON[T](cli, ctx, key, dst)` | error | 泛型一把梭复杂结构 |
| `MustGetJSON[T](cli, ctx, key, dst)` | bool | dst 已自带默认值时用 |

热路径仍推荐 `Bind[T](&atomic.Pointer[T])` — 一次绑到 atomic 指针后，业务读 `ptr.Load()` 零分配。

## SDK 双版本本地缓存（已加）

```
entry { active, pending *ConfigValue }
```

- **active** = 当前生效版本
- **pending** = 已收到但 `effective_at > now` 的未来版本

读时间感知（Get 优先级）：

```
pending(IsEffective(now)) > active(IsEffective(now)) > fallback.active
```

server 推 SCHEDULED 配置时 SDK 立刻收到 cfg；按 `EffectiveAt` 自动归位 active 或 pending 槽。
1Hz swapper 后台 ticker 到点 atomic swap pending → active；同时 `Get` 自身也是时间感知 — 即使 swapper 滞后 ≤1s，业务读也按当前 `time.Now()` 选最该生效的版本。

## v0.1 限制（生产前要补）

- proto stub 没生成（需 protoc + go-proto-gen）— 建议跑 `make proto` 一次
- service.SearchItems / GetSubscribers / SetSubscribers / GetVersion / RecentAudit /
  ListNamespace / ListVersions / GetActiveAdmin **接口已声明，repo 层未实装**
- WebSocket / SSE 通道有路由设计无具体 wire code（gorilla/websocket 待加依赖）
- admin RBAC / CSRF token 派发 stub（需对接 user-merchant-core IntrospectToken）
- Dockerfile / docker-compose 接入 待补
- 跨 server 副本 fan-out（Kafka pub/sub）待补 — 单实例 OK，多副本需对接

## 用户的 7 个需求点 ↔ 实现

| 用户需求 | 实现 |
| --- | --- |
| **定点推送** | `ReleaseStrategy.TARGETED` + `TargetedSpec.instance_ids` 严格名单 |
| **灰度推送** | `ReleaseStrategy.CANARY` + `CanarySpec{percent, target_instance_ids}` |
| **回滚** | `Rollback(toVersion)` — 复制旧 version 产新 version 号，保单调；hub 通知所有订阅 |
| **全量推送** | `ReleaseStrategy.FULL`（默认） |
| **按时生效** | `ReleaseStrategy.SCHEDULED` + `effective_at` |
| **版本管理** | `config_version` 表 append-only；`ListVersions` API + admin 详情页表格 |
| **生效时间范围检查** | `effective_at` + `expire_at` 字段；server `matchStrategy` 校验；client SDK `IsEffective` 二次校验 fallback |

## 用户后续追加的需求 ↔ 实现

| 追加需求 | 实现 |
| --- | --- |
| 本地 cache SDK | `payment-util/configcenter/client.go` — atomic.Value 整体替换 + atomic.Pointer[T] type-safe binding，纳秒级零锁读 |
| 同代码架构（fx / mTLS / etcd 自注册） | 目录布局严格对齐 card-center / kms-manage（cmd/server / internal/{service,repo,server,metrics}） |
| HTTP / WebSocket 推送 | admin web 走 WS（`/ws?namespace=`）；老客户端走 SSE long-poll；三个通道共享同一 service.WatchNamespace channel |
| 管理页面 | `internal/server/admin_html.go` 完整路由 + HTML template skeleton |
| 配置可被多个系统共用 | `config_subscription` 多对多表 + admin "新建" 表单含"应用到哪些系统"多选 |
| 按 key / 按系统展示搜索 | `/admin/items?q=...&sub=...` 全平台 item 检索 + `/admin/ns/{ns}` 按服务名 |
| **版本 diff 看差异** | `unifiedDiff()` LCS 实现；`/admin/ns/{ns}/{key}/diff?from=N&to=M` 路由；HTML 模板色标 add/del/ctx |
| accounting-admin-web / payment-admin-web 可推配置迁过来 | `README.md` 12 条迁移清单 + 三阶段灰度 SOP |

## 下一步落地清单（按优先级）

1. **proto 生成** — `make proto` 一次产 generated stub
2. **repo 实装** — 5 个 service 接口对应 GORM 实现（~150 行）
3. **gRPC handler wire** — `internal/server/grpc.go` 把 service.* 暴露到 proto 服务
4. **WebSocket wire** — `gorilla/websocket` 依赖 + `/ws` upgrade handler 转 service.WatchNamespace
5. **fx main + Dockerfile + compose** — 跟其他 11 服务一致的部署套
6. **admin auth wire** — `IntrospectToken` middleware → `WithActor(ctx)`
7. **Kafka pub/sub** — 多副本场景跨实例 fan-out
8. **Pilot 迁配置** — `payment-channel.<adapter>.timeout` 试一个低风险

预估总工作量：~3-4d 单人完成 v1.0（生产可用）。

## 文件清单

```
packages/config-center/
├── README.md                                       架构 + 三种推送协议 + 12 条迁移清单
├── STATUS.md                                       本文（功能矩阵 + 已完成 / 待办）
├── api/proto/configcenter/v1/configcenter.proto    gRPC 契约（4 策略 + 6 RPC）
├── database/metadb/init/init.sql                   5 张表
└── internal/
    ├── service/
    │   ├── service.go                              业务逻辑
    │   └── hub.go                                  fan-out + strategy 命中判断
    └── server/
        └── admin_html.go                           admin web UI + diff

packages/payment-util/configcenter/
└── client.go                                       客户端 SDK（cache + watch + Bind）
```

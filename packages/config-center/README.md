# config-center — 平台统一配置中心

跟 card-center / order-core / payment-core 等服务采用**相同的代码架构**：fx DI + assertProdSafety + etcd 自注册 + mTLS gRPC + admin HTTP + Prometheus metrics + structured zap logging。

## 目录结构（参考其他服务）

```
packages/config-center/
├── api/proto/configcenter/v1/configcenter.proto   gRPC 契约
├── cmd/server/                                     fx main + assertProdSafety
│   ├── main.go
│   └── registry.go                                 etcd 自注册（同模板）
├── internal/
│   ├── service/                                    业务逻辑
│   │   ├── service.go                             PutConfig / Rollback / GetConfig / WatchNamespace
│   │   └── hub.go                                  进程内 fan-out (Subscribe/Publish)
│   ├── repo/                                       DB CRUD（meta 库；config 量小不分片）
│   ├── server/
│   │   ├── grpc.go                                 gRPC handler，挂 service
│   │   ├── http.go                                 admin HTML UI（put/rollback/list）
│   │   └── websocket.go                            WebSocket 推送通道（浏览器 / 老客户端）
│   ├── metrics/                                    Prometheus 指标
│   └── pusher/
│       └── pusher.go                               跨副本 fan-out（Kafka pub/sub）
├── database/metadb/init/init.sql                   4 张表 schema
├── Dockerfile                                      sibling staging 模板
└── README.md                                       本文
```

## 三种推送协议

### 1. gRPC server-stream（首选，服务间）

```
client.WatchConfig(WatchConfigRequest{namespace, instance_id, since_version})
  → server stream WatchConfigEvent
```

mTLS 全栈复用，等同 card-center / kms-manage。客户端 SDK 在
`payment-util/configcenter/client.go`。

### 2. WebSocket（浏览器 / admin web 用）

```
ws://config-center:9690/ws?namespace=card-payment&instance_id=admin-ui
```

服务端用 gorilla/websocket 把 service.WatchNamespace 的 channel 转成 WS frame。
admin web（accounting-admin-web / payment-admin-web）通过这个通道接配置变更，
不需要 gRPC stub。

### 3. HTTP long-poll / SSE（老客户端兜底）

```
GET /api/v1/configs/watch?namespace=...&since_version=N&timeout=30s
```

服务端 hold 30s（server-side timeout），有事件就返；超时返 304 Not Modified
带当前 since_version 让客户端继续轮询。SSE 模式（`Accept: text/event-stream`）
则保持长连接持续推。

三个协议**共享同一个 service.WatchNamespace channel**；写入流程一次扇出到三种通道。

## 数据流

```
admin (UI / API) ──HTTP/gRPC──> config-center server
                                   ↓
                         service.PutConfig
                                   ↓
                         repo.PutVersion (DB tx)
                                   ↓
                         hub.Publish(namespace, event)
                          ↓        ↓        ↓
                       gRPC     WS       SSE
                       stream   conn     long-poll
                          ↓        ↓        ↓
                    业务服务    admin    老客户端
                    SDK cache   web UI
```

## 数据模型

| 表 | 用途 | 关键字段 |
| --- | --- | --- |
| config_namespace | 一组相关配置（= 服务名） | name, owner |
| config_item | (ns, key) 当前指针 → active_version_id | namespace, key_name, active_version |
| config_version | 不可变版本历史；rollback/list 来源 | version, value, format, strategy, strategy_spec, effective_at, expire_at |
| config_audit_log | admin 操作审计 | namespace, key, op, before/after, actor |

## 4 种发布策略

| Strategy | 行为 | 用例 |
| --- | --- | --- |
| **FULL** | 所有订阅本 namespace 的实例都收到 | 普通配置更新 |
| **CANARY** | 按比例 / 显式名单灰度 | 危险变更先 1% 灰度 |
| **TARGETED** | 严格只推 instance_ids 列表 | 单实例验证 |
| **SCHEDULED** | 等到 effective_at 才生效 | 预定凌晨 2 点切换 |

策略命中检查在 server 端 `service.matchStrategy()` 完成；客户端永远是
"server 推什么收什么"。

## 客户端 SDK 用法

```go
// service main.go：
cli, _ := configcenter.NewWithRPC(rpcStub, configcenter.Config{
    Endpoints:  []string{"etcd:///config-center"},
    Namespace:  "card-payment",
    InstanceID: serviceregistry.AdvertiseAddr(0),
    Logger:     logger,
})
defer cli.Close()

// 类型安全绑定：变更时 atomic.Pointer 自动替换
type RateLimit struct{ RPS, Burst int }
var rl atomic.Pointer[RateLimit]
configcenter.Bind(cli, "rate_limit", &rl, func() {
    logger.Info("rate_limit config changed")
})

// 业务热路径（纳秒级，无锁）：
limit := rl.Load()
if limit != nil { /* use limit.RPS */ }

// 简单读：
val, err := cli.Get(ctx, "feature.new_router")
if err != nil { val = defaultVal }
```

SDK 关键特性：
- 启动期同步拉 snapshot，失败用 fallback 默认值不阻塞
- `atomic.Pointer[T]` 整体替换，业务读零锁纳秒级
- 生效时间窗自动校验（now < effective_at 或 expire 已过自动 fallback 上一个有效值）
- Watch 自动重连（exponential backoff capped 30s）+ since_version resume 不丢事件
- server 全挂时 cache 兜底；业务永远有值

## 配置迁移：admin-web 推送配置 → config-center

### 现状

`accounting-admin-web` 和 `payment-admin-web` 当前承担两类配置：

1. **静态配置**（部署时确定，重启生效）— 留在 yaml / env，不动
2. **动态可推配置**（运行时改，热生效）— 这部分迁到 config-center

### 可迁移清单（盘点后）

| 服务 | 可推配置 | namespace.key | 类型 |
| --- | --- | --- | --- |
| payment-core | 渠道路由权重 | `payment-core` / `routing.weights` | json |
| payment-core | 风控 fail-policy | `payment-core` / `risk.fail_policy` | plain |
| payment-channel | 渠道 timeout | `payment-channel` / `<adapter>.timeout` | int |
| payment-channel | 渠道是否启用 | `payment-channel` / `<adapter>.enabled` | bool |
| risk-manage | 风控规则阈值 | `risk-manage` / `rule.<name>.threshold` | int |
| order-core | refund 单笔上限 | `order-core` / `refund.max_amount` | int |
| order-core | webhook 重试次数 | `order-core` / `webhook.max_retries` | int |
| accounting-system | trial-balance 触发频率 | `accounting-system` / `daycut.interval` | duration |
| api-gateway | per-merchant rate limit | `api-gateway` / `ratelimit.<mch_id>` | json |
| card-payment | bulkhead per-merchant | `card-payment` / `bulkhead.per_merchant_max` | int |
| card-payment | network adapter timeout | `card-payment` / `network.<n>.timeout` | duration |
| card-center | per-user tokenize 限流 | `card-center` / `ratelimit.tokenize` | json |

### 迁移步骤（每条配置）

1. **加 config-center 兜底**：
   ```go
   // 在服务 main.go 启动期
   var rateLimitCfg atomic.Pointer[RateLimitConfig]
   defaultRL := &RateLimitConfig{RPS: 100, Burst: 200}
   rateLimitCfg.Store(defaultRL)
   configcenter.Bind(cli, "ratelimit", &rateLimitCfg, nil)
   ```

2. **业务路径改读 atomic.Pointer**：
   ```go
   // 之前：limit := s.cfg.RateLimit.RPS
   // 之后：
   c := rateLimitCfg.Load()
   limit := c.RPS  // 永不为 nil（启动期 Store 了 default）
   ```

3. **admin-web 不再写 yaml**：原 admin-web 上的"修改 rate limit" 按钮改成
   POST /api/v1/configs/{namespace}/{key} 调 config-center.PutConfig；
   不再下发到具体服务的 yaml / env。

4. **灰度上线**：配置先 SCHEDULED 推（凌晨切换）+ CANARY 5% → 50% → FULL；
   出问题随时 Rollback 一键切回上版。

### 三个阶段

**阶段一（v1，本 commit）**：config-center 服务 + SDK 上线，但**不强制使用**。
existing 服务继续读 yaml；config-center 跑空也无业务影响。

**阶段二（v2）**：选 1-2 个低风险配置（payment-channel.timeout / api-gateway 限流）
迁过来作为 pilot，跑 1 周稳定后再扩。

**阶段三（v3）**：全部 12 项可推配置迁完。admin-web 改 UI 直接对接 config-center。
原来 yaml 里的同名 key 删掉（或保留作 server 启动期 fallback）。

## 部署

```yaml
# docker-compose 添加
config-center:
  build: ../../config-center
  image: config-center:local
  depends_on:
    db-init: {condition: service_completed_successfully}
  ports: ["9690:9690", "9691:9691"]   # 9690 gRPC; 9691 HTTP / WS
  environment:
    CONFIGCENTER_DATABASE_DSN: "root:password@tcp(shared-meta:3306)/config_center_meta?..."
    CONFIGCENTER_REGISTRY_ENDPOINTS: "etcd:2379"
```

## 上线 checklist

- [ ] proto 生成 stub（make proto）
- [ ] DB schema 灌入 shared-meta（generate-shared-init.sh 会自动 pick up）
- [ ] mTLS cert / KMS bearer 配齐
- [ ] assertProdSafety 跟其他服务一致：拒 insecure / dev_mode 进 prod
- [ ] Prometheus alert 加 config_center.* 指标
- [ ] admin-web 集成 config-center 的 client SDK 改写 UI 后台
- [ ] 灰度 SOP 写到 docs/CONFIG_CENTER_RELEASE_SOP.md

## 参考 / 一致性

- gRPC 模板：跟 `kms-manage` 一致（mTLS + ClientIdentity + Bearer 三层）
- etcd 自注册：跟 `serviceregistry.Registrar` 同
- 客户端 SDK：跟 `payment-util/audit/kafkago` 同包风格
- shadow 流量：本服务**不参与** shadow（管理面 service，无业务流量）

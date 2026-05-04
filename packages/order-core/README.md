# order-core

订单核心服务（Stripe Payment Intent 风格）。

## 系统架构

```
   ┌──────────────┐    ┌───────────────┐    ┌─────────────────────┐
   │ Cashier / UI │───▶│  order-core   │───▶│    payment-core     │───▶ Stripe / Adyen
   │  (web/iOS/   │    │  (this repo)  │    │ (xiongwp/payment-   │───▶ Alipay / WeChat
   │   Android/   │    │               │    │       core)         │───▶ GCash / GrabPay
   │    server)   │◀───│               │◀───│                     │───▶ ShopeePay / Bank
   └──────────────┘    └───────────────┘    └─────────────────────┘
                              │ │                       │
                              │ └─ webhook/notify ──────┘
                              │ (HTTP/APNs/FCM/WS/MQ)
                              ▼
                       ┌──────────────┐  ┌──────────────┐
                       │ MySQL 10×10  │  │ MySQL meta   │
                       │ payment_intent│ │ leaf_alloc   │
                       │ charge       │  │              │
                       │ refund       │  │              │
                       │ pay_action   │  │              │
                       │ notify_log   │  │              │
                       │ exception_   │  │              │
                       │   case       │  │              │
                       └──────────────┘  └──────────────┘
```

**职责划分**：

| 层 | 仓库 | 职责 | 是否有 DB |
|---|---|---|---|
| `order-core` | 本仓库 | PaymentIntent 状态机、退款编排、业务幂等、对账 worker、通知编排、收银台 API | ✅ MySQL 10×10 + meta |
| `payment-core` | `xiongwp/payment-core` | **只负责集成 payment-channel**：按 payment_method 路由 → 调用 adapter → 规范化返回（无业务状态） | ❌ 无状态 |
| `payment-channel` | `xiongwp/payment-channel`（或多个） | 真实对接 Stripe/Adyen/Alipay/GCash/GrabPay/ShopeePay/Bank API；落渠道 request/response 流水 + 幂等表 | ✅ MySQL 10×10（`pi_id` 分片，与 order-core 同分片号） |

`order-core` 通过 `internal/channel.PaymentChannel` interface 调用 payment-core；
本仓库自带 `service.MockPaymentCoreChannel` 供脱机开发使用。

## 目录

```
├── api/proto/order/v1/order.proto       PaymentIntent / Charge / Refund / PayAction / Webhook 服务
├── cmd/
│   ├── server/                          fx 装配的入口
│   └── grpc-client/                     CLI 调试工具
├── config/
│   ├── config.yaml                      本地默认
│   └── config.docker.yaml               docker-compose 用
├── database/
│   ├── orderdb/templates/schema.sql     分片表模板
│   ├── orderdb/scripts/generate.sh      生成 0..9_init.sql
│   ├── orderdb/init/                    生成产物
│   ├── metadb/init/init.sql             leaf_alloc
│   └── migrations/                      后续 schema 变更（apply.sh 应用到全分片）
├── internal/
│   ├── channel/                         PaymentChannel + Notifier interface
│   ├── crypto/                          AES-GCM 字段级加密
│   ├── domain/                          PI / Charge / Refund / PayAction / NotifyLog / ExceptionCase
│   ├── idgen/                           Leaf Segment id 生成
│   ├── metrics/                         Prometheus
│   ├── repo/                            分片仓储 + DBManager + ZapGormLogger（每条 SQL 带耗时）
│   ├── server/                          gRPC adapter + interceptors（auth/limit/metrics/recover）
│   ├── service/                         业务层 + 各 worker
│   └── sharding/                        10×10 路由器
├── docker-compose.yml                   一键起 10 MySQL + meta + 服务
├── Dockerfile / Makefile / go.mod
```

## 快速开始

```bash
# 1. 安装 protoc 插件 + 生成 stubs
make install-tools && make proto

# 2. 生成 10 库初始化 SQL
make gen-sql

# 3. 编译
make build

# 4. 一键起整个栈（10 MySQL + 服务）
docker-compose up -d
```

CLI 调试：

```bash
go run ./cmd/grpc-client -addr 127.0.0.1:9091 \
  create -mch m1 -biz biz1 -amount 1000 -currency USD -idem k-1
# → 拿到 pi_xxx

go run ./cmd/grpc-client -addr 127.0.0.1:9091 \
  confirm -id pi_xxx -pm BALANCE
# → mock 返回 OTP next_action

go run ./cmd/grpc-client -addr 127.0.0.1:9091 \
  confirm-action -id pi_xxx -action act_xxx -k otp=123456
# → 验证通过 → PI succeeded

# 退款（按比例自动拆，组合支付下生成多笔 refund）
go run ./cmd/grpc-client -addr 127.0.0.1:9091 \
  refund -pi pi_xxx -amount 500 -reason requested_by_customer

# 模拟 webhook（payment-core 收到渠道异步回调后转发过来）
go run ./cmd/grpc-client -addr 127.0.0.1:9091 webhook \
  -channel payment-core -event_id e1 -event charge.succeeded -pi pi_xxx -charge ch_xxx
```

## 关键设计点

### 状态机（payment_intent）

```
created ─→ requires_action ⇆ requires_action(自环换卡)
   │           │
   │           ↓
   ├──→ processing ──→ succeeded
   │           │           │
   │           ↓           ↓
   │         failed     refunding (refund_phase 字段)
   │                     │
   │                     ├─ partially_refunded ──→ refunding
   │                     ├─ fully_refunded (终态)
   │                     └─ refund_failed ──→ refunding
   │
   └──→ canceled
```

支付 status 与退款 refund_phase 是两个独立字段，互不污染。

### 分片

- 10 库 × 10 表 = 100 全局分片
- `business_id`（兜底 `mch_id`）哈希定分片
- `pi_id` 前缀编码分片位（`pi_<dbIdx><tblIdx><seq>`），`pi_id` / `business_id` 路由结果一致
- Charge / Refund / PayAction / NotifyLog / ExceptionCase 与父 PI 同分片

### 退款

- 用户主动退款：`pending → succeeded / failed`，失败后用户可新建一笔
- 系统补偿退款（`auto_compensate=1`）：`RefundRetryWorker` 无限重试到 succeeded
- 组合支付：`RefundService.CreateBundle` 按 `Charge.AmountCaptured` 比例自动拆 N 笔

### 幂等

`(mch_id, idempotency_key)` 唯一索引；`PaymentIntentService.Create` 命中即返回首次结果。

### Cron Workers

| Worker | 作用 |
|---|---|
| `ExpireWorker` | PI 过期 → CANCELED |
| `ChargeExpireWorker` | Charge 过期 → expired + PI failed |
| `RefundRetryWorker` | 退款重试（auto_compensate 永不放弃） |
| `NotifyRetryWorker` | 通知重试，指数退避 |
| `ReconcileWorker` | 长时间 pending 的 Charge → channel.Query 矫正 |

### 渠道挑战 (PayAction)

`payment-core` 返回 `RequiredAction` → order-core 自动落 `PayAction(pending)` →
收银台根据 `action_type` 渲染（OTP 输入 / 3DS 跳转 / 密码键盘 / QR 码 / App 跳转） →
用户完成后调 `ConfirmPaymentAction` 把结果回传 → 验证通过推进 PI。

### 字段加密

`internal/crypto.FieldCipher` AES-GCM 为敏感字段加密。`ClientSecret` /
`PayAction.ExpectedSecret` / `Charge.PaymentMethodRef` 应在落库前 Seal、读出后 Open。
开发用 `NoopCipher`，生产用 KMS 注入的 32 字节 key。

### 通知编排

`channel.NotifyRouter` 按 `ClientType` 选通道：

```
server   → http
web      → websocket → http
ios      → apns      → http
android  → fcm       → http
miniapp  → websocket → http
internal → mq
```

每次推送写 `notify_log_XX`：状态、重试次数、HTTP 状态码、失败原因。

### Observability

- gRPC 拦截器：`recover` / `metrics` / `rate_limit` / `auth(bearer)` 链
- `:9090/metrics` Prometheus
- `:9090/healthz` 健康检查
- 每条 SQL 通过 GORM logger 带 `elapsed_ms / rows / sql` 输出到 zap

## 配置示例

```yaml
server: { grpc_port: 9091 }
metrics: { addr: ":9090" }
sharding: { db_count: 10, table_per_db: 10 }
expire_worker: { interval: 60s, limit: 200 }
refund_retry_worker: { interval: 60s, limit: 200 }
channel: { default_name: payment-core }
auth: { tokens: ["dev-token-1"] }    # 非空开启 bearer 鉴权
rate_limit: { rps: 1000, burst: 2000 } # 0 禁用
database:
  meta:  { name: order_meta, dsn: "..." }
  shards: [...]
```

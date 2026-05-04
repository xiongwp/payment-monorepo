# order-core

Stripe 风格的 PaymentIntent / Charge / Refund 编排服务。前端收单 → FSM 推进 → 调 payment-core 实际扣款 → 通知商户 → 记账。

## 定位

```
商户 SDK / admin-web
        ↓ gRPC
┌───────────────────┐
│    order-core     │  ← 本仓
└───────┬───────────┘
        │ gRPC (channel abstraction)
        ↓
  payment-core  → payment-channel → 各外部渠道
        │
        │ gRPC (accounting)
        ↓
  accounting-system
```

- **上游**：商户 SDK / admin-web / 直接的 API 调用
- **下游**：payment-core（路由扣款）、accounting-system（记账）、webhook 商户回调

## 核心领域

| 模型 | 表 | 语义 |
|---|---|---|
| PaymentIntent | `payment_intent_NN`（分片 100 表） | 一笔支付意图；Stripe 风格 FSM：requires_confirmation → requires_action → processing → succeeded / failed / canceled |
| Charge | `charge_NN` | PI 下的实际扣款记录；同一 PI 可多次 retry，只有一次会 succeeded |
| Refund | `refund_NN` | 退款；按 charge 挂载 |
| PayAction | `pay_action_NN` | 3DS / OTP / app redirect 这种 `requires_action` 对应的一次行动 |
| InboundWebhook | `inbound_webhook` | 渠道回调去重（按 `(channel_name, event_id)` 唯一） |
| AccountingOutbox | `accounting_outbox_NN` | 记账事件 outbox，异步投递到 accounting-system，幂等靠 `request_id` |
| NotifyLog | meta: `webhook_deliveries` | 商户回调投递记录（失败指数退避重试）|

## 分片

`pi_{dbIdx:1d}{tblIdx:02d}{seq}` 格式 → 10 库 × 10 表 = 100 分片。Charge / Refund / AccountingOutbox 按 `payment_intent_id` 路由，和 PI 同分片。

## 关键 worker（内置 cron）

- **ExpireWorker**：扫过期 PI（`requires_action` 超时）→ cancel
- **RefundRetryWorker**：渠道侧 pending 退款重新查询
- **ChargeExpireWorker**：渠道侧长时间 pending charge 调 Query 查真实状态
- **ReconcileWorker**：`pending/processing` charge 调 payment-core 查真实状态
- **WebhookRetryWorker**：商户回调失败重试
- **AccountingOutboxWorker**：outbox `pending` 行投递 accounting-system（轮询 5s）
- **AccountingOutboxArchiver**：7 天前 `sent` 行归档 + `failed` dead-letter 计数

> 日切对账（charges vs accounting fleet 余额）由独立的 reconplatform 系统统一处理，
> order-core 不再内置 ReconciliationWorker；系统内严禁服务间 HTTP 互调（仅
> admin 控制台 / 配置推送除外）。

## 本地运行

依赖：kms-manage / payment-core / payment-channel / risk-manage / accounting-grpc-api 同级检出（`../xxx`，见 go.mod replace）。

```bash
# 单元测试
go test ./internal/...

# e2e（对接 docker 化的 stack）
go run ./cmd/e2e-accounting
```

## 常用操作

```bash
# gRPC CLI 调试
go run ./cmd/grpc-client -addr 127.0.0.1:9091 create -mch m1 -amount 10000
go run ./cmd/grpc-client -addr 127.0.0.1:9091 confirm -id pi_xxx -pm GCASH

# 查 outbox 堆积情况
docker exec shared-shard-5 mysql -uroot -ppassword -e \
  "SELECT status, COUNT(*) FROM order_db_5.accounting_outbox_51 GROUP BY status"

# 按 trace_id grep 日志跨服务拼路径
docker logs order-core 2>&1 | grep trace_id=abc123
```

## 依赖约束

- `customer_id` 数字段必须 ∈ `[100000000, 899999999]`（accounting-system 保留 `[1,100]` 给系统账户、`[101,999]` 给 channel business_type）
- 所有金额（`amount`, `amount_captured`, `AccountingOutbox.Amount`）都是 **ISO minor units**（PHP cents）
- `payment_method` 在各处大小写敏感：`GCASH`（大写）
- `PI.Metadata["country"]="PH"` 是必填项（payment-core 路由规则按 country 匹配）

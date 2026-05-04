# payment-channel

外部支付渠道 adapter 层。gRPC server 接受 payment-core 的请求，按 adapter 名称分发到具体渠道（GCash / Maya / GrabPay / ShopeePay / Coins.ph / InstaPay / Pesonet / PayMongo / ...）的 HTTP/SDK 实现。

## 定位

```
payment-core  ─gRPC──→  payment-channel  ─HTTP──→  外部渠道 API
                            │
                            ├─ kms-manage  密钥解密（渠道 API key）
                            └─ MySQL paychan_meta / paychan_db_N
                               （acquirer_tx 幂等 + channel_token 缓存）
```

## 核心概念

### Adapter
每个外部渠道一个 `internal/adapter/<name>/` 包，实现：
- `Charge(req) → resp`：发起扣款（可能是 requires_action）
- `Query(req)`：查询交易状态
- `Refund(req)`：退款
- `ParseWebhook(headers, body)`：解析异步通知 → 规范化 `WebhookEvent`

### AcquirerTx（幂等锁）
`paychan_db_N.acquirer_tx_NN` 按 `(adapter, idempotency_key)` 唯一约束。同一 key 重复请求回放首次响应，避免对外部渠道发二次真实调用。分片按 `payment_intent_id`。

### ChannelToken（OAuth 2 缓存）
部分渠道（GCash / GrabPay）的 access token 有有效期，用 `paychan_meta.channel_token` 缓存 + 定时刷新。

## 接口

- gRPC `AcquirerService`（:9490）：`Charge / Query / Refund / ParseWebhook`
- Mock 服务器（`cmd/mockserver`）：给本地 e2e 测试用，模拟外部渠道行为

## 分片

10 库 × 10 表 = 100 分片。`acquirer_tx` 按 `payment_intent_id` 路由。

## 本地运行

```bash
# 单测
go test ./internal/...

# mock server
go run ./cmd/mockserver
```

## 新增 adapter

1. 新建 `internal/adapter/<name>/<name>.go`，实现 `channel.Adapter` 接口
2. 在 `cmd/server/main.go` 的 adapter registry 里注册
3. payment-core 路由规则加一条 `payment_method: XXX adapter: <name>`
4. 渠道 API key 通过 kms-manage 加密后写 yaml（`kms:v1:...` 前缀触发透明解密）

## 依赖约束

- adapter 名称与 payment-core `routing.adapter` 字段严格匹配（区分大小写）
- `ParseWebhook` 必须幂等 — order-core 会在 inbound_webhook 层做 `(channel, event_id)` 去重但 adapter 内部最好也设计成幂等
- 新渠道的 `external_ref_no` / `charge_id` 映射必须写入 AcquirerTx，否则异步通知找不到对应的 order

# payment-channel

`payment-channel` 是菲律宾本地收单通道适配层。目前接入了 **15 家**主流
渠道：

| 类别 | 渠道 |
|---|---|
| 电子钱包 | GCash、Maya、GrabPay、ShopeePay、Coins.ph |
| 银行实时转账 | InstaPay、PESONet、BDO、BPI、Metrobank、Landbank |
| PSP / 聚合 | PayMongo、Xendit、Dragonpay |
| BNPL | BillEase |

职责单一：

1. 把 payment-core 的统一 `AcquirerService` gRPC 请求路由到具体的 PH adapter
   （GCash / Maya / GrabPay / Coins.ph / InstaPay / PESONet）；
2. 每次出站请求落一行 `acquirer_tx`（save-first-then-call），保留完整 request /
   response snapshot 作为审计、对账依据；
3. 用 `UNIQUE(adapter, idempotency_key)` 做幂等兜底，重复请求直接回放首次响应，
   **绝不向第三方重复下单**；
4. 接收各渠道 webhook，验签后写 `webhook_raw` 做幂等，再规范化成 `WebhookEvent`
   转发给 order-core 的 `WebhookService.Ingest`。

## 目录

```
├── api/proto/channel/v1/channel.proto     AcquirerService gRPC 面
├── cmd/
│   ├── server/                             fx 装配入口
│   └── grpc-client/                        CLI 调试（charge / refund / query）
├── config/config.yaml                      本地默认
├── database/
│   ├── paychandb/templates/schema.sql      acquirer_tx / webhook_raw / channel_token
│   ├── paychandb/scripts/generate.sh       产出 0..9_init.sql
│   └── metadb/init/init.sql                leaf_alloc（号段）
├── internal/
│   ├── channel/                            Adapter interface、failure_code、sign helpers
│   ├── domain/                             AcquirerTx / WebhookRaw / ChannelToken
│   ├── adapter/
│   │   ├── gcash/          Alipay+ Partner API（RSA-SHA256）
│   │   ├── maya/           Checkout v2（Basic Auth）
│   │   ├── grabpay/        OTC（HMAC-SHA256 X-GID-AUX-*）
│   │   ├── coinsph/        Merchant Checkout（SHA1 shared secret）
│   │   ├── instapay/       UnionBank Partner InstaPay（OAuth2 + mTLS）
│   │   ├── pesonet/        UnionBank Partner PESONet（OAuth2 + mTLS，批量）
│   │   ├── shopeepay/      SeaMoney Partner API（HMAC-SHA256 canonical）
│   │   ├── billease/       BNPL（OAuth2 + hosted checkout）
│   │   ├── bdo/            BDO Unibank Partner API（OAuth2 + HMAC-SHA256）
│   │   ├── bpi/            BPI Open Finance（OAuth2 + JWS-style HMAC）
│   │   ├── metrobank/      M Developer Portal（OAuth2 + IBM gateway + HMAC）
│   │   ├── landbank/       ePay（Basic Auth + MD5 digest，hosted redirect）
│   │   ├── paymongo/       PayMongo Sources + Payments（Basic Auth）
│   │   ├── xendit/         Xendit Invoices（Basic Auth + x-callback-token）
│   │   └── dragonpay/      Dragonpay collect/refund（SHA1 digest，OTC 聚合）
│   ├── repo/                               10×10 分片仓储 + ZapGormLogger
│   ├── service/                            AcquirerService / WebhookService / CallRetryWorker
│   ├── server/                             gRPC adapter + /wh/{adapter} HTTP
│   ├── sharding/                           10×10 路由器（与 order-core 一致）
│   ├── idgen/                              Leaf Segment
│   ├── crypto/                             AES-GCM FieldCipher
│   └── metrics/                            Prometheus paychan_*
├── Dockerfile / Makefile / go.mod
```

## 快速开始

```bash
make install-tools && make proto      # 生成 gRPC stub
make gen-sql                          # 生成 10 库分片 SQL
make build                            # 编译
./bin/payment-channel                 # 运行（读 ./config/config.yaml）
```

## 关键设计

### save-first-then-call

所有 adapter 的出站请求都：
1. 写 `acquirer_tx` 一行 `state=pending` + request snapshot；
2. 发 HTTP；
3. 回来后 `UPDATE` 同一行为 `succeeded / failed` + response snapshot。

HTTP 5xx / 超时 / 网络错误都会落 `failed` + `next_retry_at`，`CallRetryWorker`
定时扫并按原 `idempotency_key` 重放（命中幂等表时直接返回首次响应，不会
重复下单）。

### 幂等键

`idempotency_key = sha256(pi_id + ":" + action)`，由 payment-core 透传。
`UNIQUE(adapter, idempotency_key)` 在 MySQL 1062 冲突时我们捕获并回放旧行。

### 分片键

与 order-core 一致：`pi_id` 前缀编码分片号（`pi_<db><tbl><seq>`），用同一套
`sharding/router.go`，保证同一 payment 在两仓落同一逻辑分片号，便于跨仓 join。

### 无业务状态

本仓不维护 PI / Charge / Refund 的生命周期，业务决策完全交给 order-core。
`acquirer_tx.state` 只有 pending / succeeded / failed 三态，指代「本次 HTTP
调用有没有走完」，与 payment 业务层状态机**无关**。

### 失败码归一

`channel/failure_code.go` 提供 `MapFailure(adapter, raw)`，把各家原始码归并到
order-core 约定的 7 个规范码（`card_declined` / `insufficient_funds` /
`risk_blocked` / `auth_failed` / `expired` / `channel_unavailable` / `unknown`）。

### Webhook 安全链

```
POST /wh/{adapter}
  ├─ adapter.ParseWebhook → SignatureOK?
  │                      └─ no → 403，不转发
  └─ UNIQUE(dedupe_key) → 重复直接 200 nop
  └─ Forwarder.Forward → order-core 的 WebhookService
  └─ MarkForwarded(err)
```

### Observability

- gRPC 拦截器：`recover → metrics → rate_limit → auth`
- Prometheus：`paychan_acquirer_call_total{adapter,action,result}`、
  `paychan_webhook_received_total`、`paychan_idempotent_replay_total` 等
- 每条 SQL 通过 `ZapGormLogger` 打 `sql + rows + elapsed_ms`

## 端到端联调

```bash
# 1. 起 10 MySQL（docker-compose 或本机）
# 2. 建库
mysql < database/metadb/init/init.sql
for f in database/paychandb/init/*.sql; do mysql -h 127.0.0.1 -P 3406 < "$f"; done

# 3. 启动
make run

# 4. 打一笔 charge（用 mock adapter 或真 GCash sandbox 凭据）
go run ./cmd/grpc-client -addr 127.0.0.1:9092 charge \
  -adapter gcash -pi pi_4371234560001 -amount 10000

# 5. 模拟收到渠道回调
curl -X POST 'http://127.0.0.1:9192/wh/gcash' \
  -H 'Signature: ...' \
  -d '{"paymentRequestId":"pi_...", "paymentStatus":"SUCCESS", ...}'
```

## 运维

- 扩 MySQL 分片：增加 `database.shards`，重新跑 `make gen-sql` 并 apply。
- 上线新 adapter：在 `internal/adapter/<name>/` 实现 `channel.Adapter`，
  `cmd/server/main.go` 里 `reg.Register(...)` 一行即可。
- 关闭某 adapter：在 `config.yaml` 清空它的凭据或直接把 `reg.Register(...)` 注释掉。

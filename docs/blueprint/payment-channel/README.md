# payment-channel blueprint

> **本目录是 payment-channel 仓库的骨架副本**，放在 `order-core` 的 `docs/`
> 下只是因为本 session 无权直接写入 `xiongwp/payment-channel`。把这个目录
> 整体拷贝到新仓库根目录就可以开跑：
>
> ```bash
> # 在 xiongwp/payment-channel 新 repo 里
> cp -r path/to/order-core/docs/blueprint/payment-channel/* .
> go mod init github.com/xiongwp/payment-channel
> make proto && make gen-sql
> ```

## 目录

```
payment-channel/
├── api/proto/channel/v1/channel.proto
├── database/paychandb/templates/schema.sql    渠道调用流水 + 幂等 + 原始 webhook
├── internal/
│   ├── channel/                                Adapter interface（被 6 个 adapter 实现）
│   ├── domain/                                 AcquirerTx / WebhookRaw / IdempotencyEntry
│   ├── adapter/
│   │   ├── gcash/          菲律宾国民钱包（partnerId + paymentRequestId）
│   │   ├── maya/           Maya Checkout v2（Basic Auth pubkey/seckey）
│   │   ├── grabpay/        Grab OTC（HMAC-SHA256 X-GID-AUX-*）
│   │   ├── coinsph/        Coins.ph Merchant API（SHA1 shared secret）
│   │   ├── instapay/       UnionBank Partner InstaPay（OAuth2 + mTLS）
│   │   └── pesonet/        UnionBank Partner PESONet（OAuth2 + mTLS，批量）
│   ├── repo/                                   AcquirerTxRepo / IdempotencyRepo
│   ├── service/                                AcquirerService（save-first-then-call）
│   └── sharding/                               与 order-core 一致：10×10
└── ...
```

## 核心不变量

1. **落库早于调用**：所有 adapter 的每个出站请求都先在 `acquirer_tx` 落一行
   `state=pending` + 请求 snapshot，再发 HTTP。返回后同行 UPDATE 成
   `succeeded / failed`，写响应 snapshot。**HTTP 超时也落 failed 行**（重试
   时通过幂等表回放，见下）。

2. **幂等表兜底**：`UNIQUE(adapter, idempotency_key)`。
   `idempotency_key = sha256(pi_id + ":" + action)`，由 payment-core 透传。
   写冲突 → 读旧记录直接回放，**不向第三方重发**。

3. **分片键与 order-core 一致**：`pi_id` 前缀携带分片号（`pi_<db><tbl>...`），
   `payment-channel` 用同一套 `sharding/router.go`，保证同一笔支付在两仓
   落同一逻辑分片号。

4. **无业务状态**：adapter 只记录「我调用第三方发生了什么」，不维护 PI /
   Charge / Refund 状态。业务决策留给 order-core。

## 详细文档

同目录 `PAYMENT_CHANNEL_PH_BLUEPRINT.md` 汇总了六个 PH 渠道的 endpoint / 鉴权 /
请求响应 / webhook 规则。

# GCash 端到端接入 walkthrough

首要菲律宾电子钱包。本文把 GCash 一条链路的所有组件串起来：
adapter、mockserver、sandbox config、gRPC server、admin UI，并给出一套
手动联测步骤。

> 如果只是想在单元测试里跑 GCash，直接看
> `internal/mockserver/mockserver_test.go:TestGCashAsyncWebhook`。
> 整个链路跑一遍：`make test-mock`。

## 1. 组件全景

```
 ┌────────────────────────┐      gRPC        ┌──────────────────────┐
 │ order-core / merchant  │────── Charge ───▶│ payment-core (router)│
 │ admin-web (BFF)        │                  └──────────┬───────────┘
 └────────────────────────┘                             │ gRPC Charge
                                                        ▼
                                        ┌────────────────────────────┐
                                        │ payment-channel (this repo)│
                                        │   internal/adapter/gcash   │
                                        └──────────────┬─────────────┘
                                             HTTP (BaseURL)
                                        ┌──────────────▼─────────────┐
                                        │ cmd/mockserver 模拟 GCash │
                                        │ RSA-SHA256 签名 + webhook │
                                        └──────────────┬─────────────┘
                                             HTTP webhook
                                        ┌──────────────▼─────────────┐
                                        │ payment-channel webhook http│
                                        │   /wh/gcash → WebhookService│
                                        └────────────────────────────┘
```

真实生产链路把 mockserver 换成 Alipay+ mPaaS 网关，其余不变。

## 2. GCash adapter 做了什么

`internal/adapter/gcash/gcash.go`

- **请求签名**：每个 HTTP 请求用商户 RSA 私钥对 `POST {path}\n{partnerId}.{ts}.{body}` 做 PKCS1v15-SHA256，放到 `Signature` header（格式：`algorithm=RSA256,keyVersion=1,signature=<b64>`）
- **Charge**：POST `/v1/payments/pay`；响应 `result.resultStatus=S/U/F` → Succeeded/RequiresAction/Failed；`U` 时返回 `schemeUrl` / `applinkUrl` / `normalUrl` 三种跳转
- **Refund**：POST `/v1/payments/refund`，`refundRequestId` 用幂等键做回放
- **Query**：POST `/v1/payments/inquiryPayment`
- **Webhook**：解析 `paymentStatus` = SUCCESS/FAIL/PENDING → 对应 `charge.succeeded/charge.failed/payment_intent.requires_action`
- **验签**：用配置里的 `gcash_pub_key`（GCash 公钥）验 `Signature` header

## 3. mockserver 如何"扮演" GCash

`internal/mockserver/gcash.go`

- 启动时随机生成一对 RSA-2048 密钥（也可通过 `Options.GCash.Priv` 注入固定密钥）
- 暴露 `/v1/payments/{pay,refund,inquiryPayment}` 三个 endpoint，响应结构严格匹配 Alipay+ mPaaS
- 同步返回 S/U/F 由 **scenario** 决定：
  - `amount=1` 或 `X-Mock-Scenario: success` → S（同步成功）
  - `amount=2` 或 `X-Mock-Scenario: ra` → U（返回 scheme/applink/normal 三件套）
  - `amount=3-6` → F，`resultCode` 分别映射到 `USER_PAYMENT_VERIFICATION_FAILED` / `USER_BALANCE_NOT_ENOUGH` / `RISK_REJECT` / `SYSTEM_ERROR`
  - `amount=7` 或 `X-Mock-Scenario: async_success` → 同步返回 U (processing)，`WebhookDelay` 后 POST webhook `paymentStatus=SUCCESS` 到 adapter 的 `NotifyURL`，webhook body 用 mock 的 RSA 私钥签名
  - `amount=8` → async fail（同上但 webhook=FAIL）
  - `amount=9` → mock 睡 30s，验证 adapter 超时路径
- **幂等**：同一个 `paymentRequestId` 重放返回同一个 `paymentId` + 相同响应
- **Refund over-amount**：超过 `amount` 返回 `REFUND_AMOUNT_EXCEED`

mock 的公钥通过 `srv.GCashPublicKeyPEM()` 暴露——放进 adapter 的
`Config.GCashPubKey` 才能让 `ParseWebhook` 的验签返回 `SignatureOK=true`。

## 4. 本地手动联测

```bash
# 1) 起 mockserver，顺便把 GCash 公钥打印到 stdout
make run-mock
# 把打印出来的 public key 贴进 config/config.sandbox.yaml:channel.gcash.gcash_pub_key
# （如需签出站请求，再在 channel.gcash.merchant_priv 里放一份 merchant 私钥 PEM）

# 2) 用 sandbox profile 起 payment-channel
PAYCHAN_CONFIG=sandbox make run
# 或 docker-compose up payment-channel mockserver

# 3) 用 grpc-client 发一笔 GCash Charge（本仓自带）
./bin/grpc-client charge \
   --adapter=gcash --amount=1 --currency=PHP \
   --pi-id=pi_demo_001 --idempotency-key=k_demo_001
# 期望：{"result":"succeeded", "external_ref_no":"2024033100000001..."}

# 4) 异步场景：amount=7
./bin/grpc-client charge --adapter=gcash --amount=7 --pi-id=pi_demo_002 \
   --idempotency-key=k_demo_002
# 期望：{"result":"requires_action", ...}
# ~200ms 后 payment-channel 日志里能看到 /wh/gcash 收到签名 webhook,
# WebhookService 把它落盘并推给 payment-core forwarder

# 5) 查状态
./bin/grpc-client query --adapter=gcash --external-ref-no=<paymentId>

# 6) 退款
./bin/grpc-client refund --adapter=gcash --external-ref-no=<paymentId> \
   --amount=1 --idempotency-key=k_demo_001_rfd
```

## 5. 从 admin-web 一键联测

前置条件同第 4 节。然后：

```
payment-admin-web → 渠道 → PH 渠道联测
   channel   = GCash
   scenario  = 任意（下拉列出 9 种）
   [发送测试 Charge]
```

页面会调 `payment-admin-web/backend` 的 `POST /api/channels/routes/probe`，
payment-core 根据 `payment_method=gcash` 路由到 GCash adapter，adapter 用
`channel.gcash.base_url` 把请求打到 mock。

## 6. 自动化测试覆盖

| 文件 | 覆盖 |
|------|------|
| `internal/mockserver/mockserver_test.go::TestGCashChargeSuccess` | 同步成功 + 幂等重放 |
| `...TestGCashChargeRequiresAction` | U 状态 + RedirectURL 字段 |
| `...TestGCashChargeFailures` | 4 种 FailureCode |
| `...TestGCashAsyncWebhook` | 异步 webhook → adapter ParseWebhook → 验签 |
| `...TestGCashRefund` | 部分退款 + 超额退款 |
| `internal/server/grpc_mockserver_e2e_test.go::TestPaychanE2E_RealGCash_AgainstMock` | gRPC ingress → AcquirerService → 真 adapter → mock 全链路 |

跑法：`make test-mock` 或 `go test -race ./internal/server/... ./internal/mockserver/...`

## 7. 切到真 Alipay+ mPaaS

三件事：

1. `channel.gcash.base_url` 清空（或删掉）→ adapter 用 `baseSandbox` / `baseProd`
2. `channel.gcash.partner_id` / `merchant_priv` / `gcash_pub_key` 换成 GCash Onboarding 发的
3. `channel.gcash.notify_url` 指到 payment-channel 公网暴露的 `/wh/gcash`，放到白名单

其余代码不动——adapter 的 request/response shape、签名算法、status 映射都已经
和真 GCash 对齐。mockserver 只是换了 base URL 而已。

## 8. 下一步（尚未实现）

- Alipay+ `settlementInfo` 日切对账文件接入
- 多商户（multi-merchant）GCash：目前 `channel.gcash.*` 是单租户，多商户需要
  移到 `merchant_id + partner_id` 维度的密钥库（考虑经 kms-manage 托管）
- OAuth refresh / partner cert rotation 自动化
- `app_redirect` → webhook 的端到端 smoke：需要等真 GCash app 环境

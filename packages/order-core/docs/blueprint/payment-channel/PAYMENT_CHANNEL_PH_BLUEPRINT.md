# 菲律宾渠道集成蓝图

六个主流菲律宾收单通道，每个给出：接入参考文档、base URL、鉴权、
关键 endpoint、请求/响应字段、webhook 验签、幂等键构造。

> 参考文档获取日期：2026-04（本地 WebFetch 部分源站有 CF 403，基础
> 事实以公开 SDK + 第三方 PSP 文档为准；生产接入前要用 merchant 自己
> 的 onboarding 包核对一遍）。

---

## 0. 各渠道一览

| 渠道     | payment_method | 鉴权                     | 请求风格                | 回到前端    | 最终支付完成信号          |
|---------|----------------|--------------------------|-------------------------|-------------|---------------------------|
| GCash   | `GCASH`        | partnerId + RSA          | JSON / URL redirect     | App deeplink | webhook + Query fallback  |
| Maya    | `MAYA`         | Basic Auth（pub/sec key） | JSON                    | Hosted page  | webhook (`CHECKOUT_SUCCESS`) |
| GrabPay | `GRABPAY`      | HMAC-SHA256 partnerSecret | JSON                    | Hosted page  | webhook + Query fallback  |
| Coins.ph| `COINS_PH`     | SHA1 shared secret        | Form / query string     | Hosted page  | webhook                   |
| InstaPay| `INSTAPAY`     | OAuth2 + mTLS             | JSON                    | N/A（银行端） | 同步返回 + 可选 webhook    |
| PESONet | `PESONET`      | OAuth2 + mTLS             | JSON                    | N/A（批量）   | 轮询 + EOD settlement     |

---

## 1. GCash（`internal/adapter/gcash/`）

- 官方参考：https://miniprogram.gcash.com/docs/miniprogram_gcash/mpdev/v1_pay
- 也可经由 Adyen / Checkout.com / 2C2P / EBANX 等 PSP 走统一 API。

### 鉴权
- `partnerId` 商户 ID（onboarding 时 GCash 分配）
- `merchantPrivateKey` / `gcashPublicKey` 配对，报文用 RSA 签名

### Base URL
- Sandbox `https://open-gw-pre.mpaas.cn-hangzhou.aliyuncs.com`（Alipay+ 骨干）
- Prod    `https://open-gw.mpaas.cn-hangzhou.aliyuncs.com`

### Charge (create payment)
```
POST /v1/payments/pay
Content-Type: application/json
Signature: <RSA-SHA256 base64>
```
```json
{
  "partnerId":         "MER001",
  "paymentRequestId":  "pi_4371234560001:charge",
  "paymentAmount":     { "currency": "PHP", "value": "10000" },
  "paymentMethod":     { "paymentMethodType": "CONNECT_WALLET" },
  "paymentFactor":     { "isAuthorization": false },
  "productCode":       "51051000101000100000",
  "paymentRedirectUrl":"https://api.order-core/ret",
  "paymentNotifyUrl":  "https://api.payment-channel/wh/gcash"
}
```
响应：`result.resultCode`、`normalUrl / applinkUrl / schemeUrl`。
前端用 `schemeUrl` 唤起 GCash App。

### 幂等键
`paymentRequestId = sha256(pi_id + ":" + action)[:32]`。同一 paymentRequestId
在 GCash 端天然幂等。

### 退款
`POST /v1/payments/refund` 带 `refundRequestId` + `originalPaymentRequestId`。

### Webhook
GCash 异步通知到 `paymentNotifyUrl`，HTTP POST、JSON body。
验签：`Signature` header = RSA-SHA256，公钥用 GCash 公钥。
关键字段：`paymentRequestId`、`paymentStatus=SUCCESS/FAIL`、`paymentResultInfo`。

---

## 2. Maya（`internal/adapter/maya/`）

- 官方文档：https://developers.maya.ph/
- Checkout v2 规格：https://s3-us-west-2.amazonaws.com/developers.paymaya.com.pg/checkout/v2/Checkout+API.html

### 鉴权
- HTTP Basic Auth，`Base64(apiKey + ":")`（password 留空）
- Create Payment 用 public key；Retrieve/Modify 用 secret key

### Base URL
- Sandbox `https://pg-sandbox.paymaya.com`
- Prod    `https://pg.paymaya.com`

### Create Checkout
```
POST /checkout/v1/checkouts
Authorization: Basic <base64(pubKey:)>
```
```json
{
  "totalAmount": {
    "value":    100.00,
    "currency": "PHP",
    "details":  { "subtotal": 100.00 }
  },
  "buyer": { "firstName": "Juan", "lastName": "Dela Cruz",
             "contact": { "email": "x@y.com", "phone": "+639..." }},
  "items": [ { "name": "item", "quantity": 1,
               "totalAmount": { "value": 100.00 }}],
  "redirectUrl": {
    "success": "https://api.order-core/ret?pi=pi_xxx&r=ok",
    "failure": "https://api.order-core/ret?pi=pi_xxx&r=fail",
    "cancel":  "https://api.order-core/ret?pi=pi_xxx&r=cancel"
  },
  "requestReferenceNumber": "pi_4371234560001:charge",
  "metadata": { "pi_id": "pi_4371234560001" }
}
```
响应：
```json
{
  "checkoutId":  "uuid",
  "redirectUrl": "https://payments-web-sandbox.paymaya.com/checkout?id=..."
}
```

### Retrieve / Refund / Void
- `GET /checkout/v1/checkouts/:checkoutId`
- `POST /payments/v1/payments/:paymentId/refunds`
- `POST /payments/v1/payments/:paymentId/voids`

### Webhook
`CHECKOUT_SUCCESS / CHECKOUT_FAILURE / CHECKOUT_DROPOUT`，payload 与 GET
checkout 返回同构。**v2 文档未指定签名校验头** —— 实战里用
`source_ip allowlist + requestReferenceNumber 与本地 AcquirerTx 对账`
兜底。

### 幂等键
`requestReferenceNumber = sha256(pi_id + ":" + action)[:64]`

---

## 3. GrabPay（`internal/adapter/grabpay/`）

- Merchant SDK：https://github.com/grab/grabpay-merchant-sdk
- Developer Portal：https://developer.grab.com/
- PH 地区只支持 API-only 模式。

### 鉴权
- `partnerId` / `partnerSecret`（Grab 发）+ `merchantId`
- 每个请求带：
  - `X-GID-AUX-Date`      RFC1123 时间
  - `X-GID-AUX-Signature` HMAC-SHA256(partnerSecret, <canonical string>)
  - `Authorization`       Bearer OAuth access token
- canonical string = `"POST" + "\n" + contentType + "\n" + date + "\n" + path + "\n" + base64(sha256(body))`

### Base URL
- Sandbox `https://partner-api.stg-myteksi.com`
- Prod    `https://partner-api.grab.com`

### Charge (OTC - One-Time Charge)
```
POST /grabpay/partner/v2/charge/init
```
```json
{
  "partnerTxID": "pi_4371234560001:charge",
  "partnerGroupTxID": "merchant-order-123",
  "amount": 10000,
  "currency": "PHP",
  "description": "Order 123",
  "merchantID": "MERCHANT-PH-001"
}
```
响应：`txID`、`state=PENDING`，返回 HTTP 302 跳到 Grab 中间页，最终返回
`returnUrl` + `status=SUCCESS/FAIL`。

### Refund
```
POST /grabpay/partner/v2/refund
```

### Webhook
Grab 会 POST 到预留 URL，验签：header `X-Signature` = HMAC-SHA256
(partnerSecret, body)。事件：`CHARGE_SUCCESS` / `CHARGE_FAIL` /
`REFUND_SUCCESS`。

### 幂等键
`partnerTxID = sha256(pi_id + ":" + action)[:64]`

---

## 4. Coins.ph（`internal/adapter/coinsph/`）

- REST API docs：https://docs.coins.ph/rest-api/
- Merchant Checkout：https://docs.coins.asia/v2/docs/merchant-checkouts

### 鉴权
SHA1 shared-secret digest。商户注册后拿 `merchantId` + `secretKey`。

### Base URL
- Sandbox `https://sandbox.coins.ph`
- Prod    `https://coins.ph`

### Create Checkout（GET 重定向式）
```
GET /pay?m=<merchantId>&d=<digest>&...
```
或服务端集成：
```
POST /pay/api/v3/invoices/
```
```json
{
  "merchant": "<merchantId>",
  "external_transaction_id": "pi_4371234560001:charge",
  "currency": "PHP",
  "amount": "100.00",
  "description": "Order 123",
  "callback_url": "https://api.payment-channel/wh/coinsph",
  "notes": { "pi_id": "pi_4371234560001" }
}
```
Digest：`sha1(secret + txnid + amount + currency)`，放到 `X-Coins-Signature`。

### Webhook
`POST` 到 `callback_url`，body 含 `external_transaction_id`、`status`、
`paid_at`、`amount`。验签同样走 SHA1 digest。

### 幂等键
`external_transaction_id = sha256(pi_id + ":" + action)[:40]`

---

## 5. InstaPay（`internal/adapter/instapay/`，以 UnionBank 为例）

- UnionBank Developer：https://developer.unionbankph.com/
- BancNet 官方：https://bancnet.com.ph/instapay/
- InstaPay = 24/7 实时，单笔上限 ₱50,000。

### 鉴权
OAuth2 client credentials + mTLS 双向 TLS。
```
POST /partners/sb/v1/oauth2/token
  client_id / client_secret / grant_type=client_credentials / scope=partner_instapay
→ access_token (短期，60-900s)
```
之后每个请求：
```
Authorization: Bearer <access_token>
x-ibm-client-id: <...>
x-ibm-client-secret: <...>
x-partner-id: <...>
```

### Base URL
- Sandbox `https://api-uat.unionbankph.com`
- Prod    `https://api.unionbankph.com`

### Transfer
```
POST /partners/v1/instapay/transfer
```
```json
{
  "senderRefId": "pi_4371234560001:charge",
  "tranRequestDate": "2026-04-15T02:30:00+08:00",
  "amount": { "currency": "PHP", "value": 100.00 },
  "beneficiary": {
    "accountNumber": "00000123456",
    "accountName":   "Juan Dela Cruz",
    "bankCode":      "BDO"
  },
  "sender": {
    "accountName":  "Merchant Co",
    "accountNumber":"9876543210"
  },
  "remittanceInformation": "Order 123"
}
```
响应：`referenceNumber`、`status=SUCCESS/PENDING/FAILED`。

### Query
```
GET /partners/v1/instapay/transfer/{senderRefId}
```

### Webhook
InstaPay 是同步返回，但 UnionBank 会对 `PENDING` 的单通过 `notifyUrl` 发
最终结果。验签用 `x-callback-signature` = HMAC-SHA256(clientSecret, body)。

### 幂等键
`senderRefId = sha256(pi_id + ":" + action)[:40]`

---

## 6. PESONet（`internal/adapter/pesonet/`，以 UnionBank 为例）

- UnionBank Developer：https://developer.unionbankph.com/
- PPMI 官方：https://www.philpayments.org.ph/pesonet
- PESONet = 批量清算，T+0 同日 / T+1 次日，单笔上限 ₱300,000（某些渠道更高）。

### 鉴权
同 InstaPay：OAuth2 + mTLS，scope 换成 `partner_pesonet`。

### Transfer
```
POST /partners/v1/pesonet/transfer
```
字段同 InstaPay，多一个 `valueDate: "2026-04-16"`。

### Query
```
GET /partners/v1/pesonet/transfer/{senderRefId}
```

### Webhook
清算完成后（早 9:00 / 午 1:00 / 晚 4:00 三批次）POST。事件
`status=SETTLED / RETURNED / REJECTED`。

### 幂等键
同 InstaPay。

---

## 失败码归一

每个 adapter 实现 `mapFailureCode(raw string) string`，把渠道原始码映射
到 order-core 已约定的七个规范码：

| 规范码                | 典型渠道原始码举例                                       |
|----------------------|----------------------------------------------------------|
| `card_declined`      | Stripe `card_declined`、Alipay `ACQ.SYSTEM_ERROR`        |
| `insufficient_funds` | GCash `INSUFFICIENT_BALANCE`、Maya `INSUFFICIENT_FUND`   |
| `risk_blocked`       | Maya `RISK_HIGH`、Adyen `Refused (FraudPolicy)`          |
| `auth_failed`        | OTP / 3DS 失败 / 密码错                                  |
| `expired`            | QR / OTP 过期、Maya `CHECKOUT_EXPIRED`                   |
| `channel_unavailable`| HTTP 5xx / OAuth 刷 token 失败 / 维护窗口                |
| `unknown`            | 兜底                                                     |

实现见 `internal/channel/failure_code.go`。

---

## 重试与退避

- HTTP 4xx（400/401/403/422）**不重试**，直接把错误归一后返回 payment-core。
- HTTP 5xx / 超时 / 网络错误 → 在 `acquirer_tx` 落 `failed`，
  `AcquirerService.Call` 的上层 worker（`CallRetryWorker`）按 `next_retry_at`
  重试，退避 `60 * 2^n` 封顶 1h；若成功命中幂等表，不会重复向第三方下单。
- Webhook 入 `webhook_raw` 表（`dedupe_key = sha256(channel + event_id)`）
  做幂等，10 秒内同一 `dedupe_key` 直接忽略。

---

## 本地联调

```bash
# 每个 adapter 在 internal/adapter/<name>/testdata/ 下放 mock fixture，
# 单元测试用 net/http/httptest 模拟上游。
go test ./internal/adapter/...

# 集成测试：docker-compose 起 10 MySQL + payment-channel 服务，
# 用 grpc-client 直接打一笔：
go run ./cmd/grpc-client charge -adapter gcash -pi pi_xxx -amount 10000
```

# PH 渠道接入细节

覆盖目前 `internal/adapter/` 下全部 **15 家**渠道。每一行给出：参考门户、base
URL、鉴权、关键 endpoint、webhook 验签方式、幂等键构造。

> 参考文档获取日期：2026-04。生产接入前要用 merchant 自己的 onboarding 包核
> 对一遍，银行 API 每季度小变动较多。

---

## 0. 一览表

| 渠道        | adapter 名     | 鉴权                                | 回前端形态        | 最终完成信号              |
|-------------|----------------|-------------------------------------|------------------|---------------------------|
| GCash       | `gcash`        | partnerId + RSA-SHA256              | App deeplink     | webhook + Query fallback  |
| Maya        | `maya`         | Basic Auth（pub/sec key）           | Hosted page      | webhook CHECKOUT_SUCCESS  |
| GrabPay     | `grabpay`      | HMAC-SHA256 X-GID-AUX-*             | Hosted page      | webhook + Query fallback  |
| ShopeePay   | `shopeepay`    | HMAC-SHA256 canonical partner sig   | App deeplink     | webhook PAYMENT_SUCCESS   |
| Coins.ph    | `coinsph`      | SHA1 shared secret                  | Hosted page      | webhook                   |
| InstaPay    | `instapay`     | OAuth2 + mTLS（UnionBank 托管）     | N/A              | 同步 + 可选 webhook        |
| PESONet     | `pesonet`      | OAuth2 + mTLS（批量）               | N/A              | 轮询 + EOD settlement     |
| BDO         | `bdo`          | OAuth2 + HMAC-SHA256                | N/A              | 同步 + webhook            |
| BPI         | `bpi`          | OAuth2 + JWS-style HMAC             | N/A              | 同步 + webhook            |
| Metrobank   | `metrobank`    | OAuth2 + IBM gateway + HMAC         | N/A              | 同步 + webhook            |
| Landbank    | `landbank`     | Basic + MD5 digest                  | Hosted page      | webhook                   |
| PayMongo    | `paymongo`     | Basic Auth (sk_*)                    | Hosted redirect  | webhook source.chargeable |
| Xendit      | `xendit`       | Basic Auth (xnd_*)                   | Hosted Invoice   | webhook x-callback-token  |
| Dragonpay   | `dragonpay`    | SHA1 digest                         | Hosted page / OTC | form-encoded webhook     |
| BillEase    | `billease`     | OAuth2 client credentials           | Hosted BNPL       | webhook checkout.approved |

---

## 1-6. 参见 `docs/blueprint/payment-channel/PAYMENT_CHANNEL_PH_BLUEPRINT.md`

GCash / Maya / GrabPay / Coins.ph / InstaPay / PESONet 的详细字段已在原蓝图
文档里写齐，本文档从第 7 节开始补齐新增 9 家。

---

## 7. ShopeePay（`internal/adapter/shopeepay/`）

- 门户：https://open.shopeepay.com / https://partner.shopeepay.com
- Base URL：`https://uat-partner.shopeepay.com` / `https://partner.shopeepay.com`
- 鉴权：`partner_id` + `partner_key`；每请求头 `Authorization: SHA256 Credential=<partner_id>,Signature=<hex>`，签名明文 `partner_id|timestamp|path|body`，`X-Timestamp` 放 unix 秒。
- Charge：`POST /v3/merchant-host/order/create`，返回 `deeplink` / `payment_link`。`state==SUCCESS` 时直接 `ResultSucceeded`，否则 `ResultRequiresAction`。
- Refund：`POST /v3/merchant-host/refund/create`。
- Query：`POST /v3/merchant-host/order/query`。
- Webhook：`Authorization` 头同签名方案，事件 `PAYMENT_SUCCESS` / `PAYMENT_FAILED` / `REFUND_SUCCESS`。
- 幂等：`payment_reference_id = sha256(pi_id:charge)[:40]`。

---

## 8. BillEase（`internal/adapter/billease/`）

- 门户：https://developer.billease.ph
- Base URL：`https://api-sandbox.billease.ph` / `https://api.billease.ph`
- 鉴权：OAuth2 client credentials，`POST /api/v1/oauth/token`，bearer token 内存缓存（到期前 30s 抢先刷新）。
- Charge：`POST /api/v2/checkout/create`，永远返回 `ResultRequiresAction`（BNPL 需要用户做信审 + redirect）。
- Refund：`POST /api/v2/checkout/{id}/refund`。
- Query：`GET /api/v2/checkout/{id}` → `status: pending/approved/paid/rejected/expired/cancelled`。
- Webhook：`X-BillEase-Signature` = HMAC-SHA256 hex (body, client_secret)。事件 `checkout.approved` / `checkout.paid` / `checkout.rejected` / `checkout.expired`。

---

## 9. BDO（`internal/adapter/bdo/`）

- 门户：https://api.bdo.com.ph（BDO Unibank Partner API Portal）
- Base URL：`https://api-sandbox.bdo.com.ph` / `https://api.bdo.com.ph`
- 鉴权：OAuth2 client_credentials（`/gateway/auth/oauth2/v1/token`，scope `payments`）+ bearer；请求头加 `X-BDO-PartnerID` + `X-Signature` = HMAC-SHA256 hex(partner_secret, body)。
- Charge（Direct Debit）：`POST /gateway/payments/v1/directdebit`，body 字段 `{partnerRefId, amount:{currency,value}, payer:{accountNumber,accountName}, remarks, callbackUrl}`。`status: SUCCESS/PENDING/FAILED`。
- Refund：`POST /gateway/payments/v1/refund`。
- Query：`GET /gateway/payments/v1/directdebit/{partnerRefId}`。
- Webhook：`X-BDO-Signature` 验签，body 含 `partnerRefId`, `status`, `referenceNumber`。

---

## 10. BPI（`internal/adapter/bpi/`）

- 门户：https://developer.bpi.com.ph（BPI Open Finance Portal）
- Base URL：`https://sandbox.api.bpi.com.ph` / `https://api.bpi.com.ph`
- 鉴权：OAuth2 client_credentials（scope `fund-transfer`）+ bearer；额外 JWS-style 签名：`X-BPI-Signature` = HMACSHA256Base64(client_secret, "{method}\n{path}\n{timestamp}\n{sha256Hex(body)}")`。
- Charge：`POST /open/v1/fund-transfer`。Beneficiary / sender 从 `req.Metadata` 取（`beneficiary_account_number` 等）。
- Refund：`POST /open/v1/fund-transfer/refund`。
- Query：`GET /open/v1/fund-transfer/{transactionId}`。
- Webhook：`X-BPI-Signature` 验签。

---

## 11. Metrobank（`internal/adapter/metrobank/`）

- 门户：https://mdeveloper.metrobank.com.ph（M Developer Portal）
- Base URL：`https://sandbox-api.metrobank.com.ph` / `https://api.metrobank.com.ph`
- 鉴权：OAuth2（scope `payments`）+ IBM gateway 头 `X-IBM-Client-Id` / `X-IBM-Client-Secret` / `X-Correlation-ID` + `X-Metrobank-Signature` = HMAC-SHA256 hex(partner_secret, body)。
- Charge：`POST /partners/v1/payments/debit`。`status: SUCCEEDED/PROCESSING/REJECTED`。
- Refund：`POST /partners/v1/payments/refund`。
- Query：`GET /partners/v1/payments/{bankRefNo}`。
- Webhook：同签名方案。

---

## 12. Landbank ePay（`internal/adapter/landbank/`）

- 门户：https://epay.landbank.com
- Base URL：`https://epay-test.landbank.com/api` / `https://epay.landbank.com/api`
- 鉴权：Basic Auth（merchant_id:secret_key）+ body 级 MD5 digest = `md5(merchant_id|txn_id|amount|secret_key)`。
- Charge：`POST /payment/v2/create`，返回 `paymentUrl`。恒为 `ResultRequiresAction`（hosted page）。
- Refund：`POST /payment/v2/refund`。
- Query：`GET /payment/v2/status/{txnId}`。
- Webhook：MD5 digest 验签。

---

## 13. PayMongo（`internal/adapter/paymongo/`）

- 门户：https://developers.paymongo.com
- Base URL：`https://api.paymongo.com`（sandbox/prod 靠 sk_ 前缀区分）
- 鉴权：Basic Auth，`Authorization: Basic base64(sk_:)`（password 留空）。
- Charge flow：Source → Payment。`POST /v1/sources` 返回 `data.id` + `data.attributes.redirect.checkout_url`。`source.chargeable` webhook 到达后再走 `/v1/payments`（生产建议 payment-core 侧完成此 hop，本仓目前只做到 Source 阶段，返回 RequiredAction）。
- `type`：从 `req.Metadata["paymongo_source_type"]` 取，支持 `gcash / grab_pay / card / dob / paymaya`。
- Refund：`POST /v1/refunds`。
- Query：`GET /v1/sources/{id}`。
- Webhook：`Paymongo-Signature: t=<ts>,te=<testsig>,li=<livesig>`，HMAC-SHA256(`${t}.${body}`, webhook_secret)。事件 `source.chargeable` / `payment.paid` / `payment.failed` / `payment.refunded`。

---

## 14. Xendit（`internal/adapter/xendit/`）

- 门户：https://developers.xendit.co
- Base URL：`https://api.xendit.co`（sandbox/prod 靠 xnd_ 前缀区分）
- 鉴权：Basic Auth，`Authorization: Basic base64(secret_key:)`。
- Charge：`POST /v2/invoices`，body `{external_id, amount, payer_email, success_redirect_url, failure_redirect_url, payment_methods, invoice_duration}`。`payment_methods` 从 `req.Metadata["xendit_payment_method"]` 取；默认全量（`GCASH`/`GRABPAY`/`PAYMAYA`/`QRPH`/`CARDS`）。
- Refund：`POST /refunds`。
- Query：`GET /v2/invoices/{id}` → `PENDING/PAID/SETTLED/EXPIRED`。
- Webhook：`x-callback-token` header 与配置值做 constant-time 比对（Xendit 不用 HMAC）。

---

## 15. Dragonpay（`internal/adapter/dragonpay/`）

- 门户：https://dragonpay.ph（Collect API / Refund API）
- Base URL：`https://test.dragonpay.ph` / `https://gw.dragonpay.ph`
- 鉴权：SHA1 digest（`sha1(merchantid:txnid:amount:ccy:description:merchantkey)`），放 body `Digest` 字段。
- Charge：`POST /api/collect/v1/{txnid}/post`。`ProcId` 从 `req.Metadata["dragonpay_procid"]` 取，默认 `GCSH`。支持的 ProcId：
  - 电子钱包：`GCSH` / `GRPY` / `MAYA` / `SHPY`
  - 网银：`BDO` / `BPI` / `BOG`(BPI online) / `MBTC` / `UBPB` / `PNBB` / `RCBC` / `SEC` / `CBC`
  - OTC：`ECPY` (ECPay) / `711` (7-Eleven) / `LBC` / `MLHUILLIER` / `CEBL` (Cebuana) / `PLWN` (Palawan)
- Refund：`POST /api/refund/v1/post`。
- Query：`GET /api/collect/v1/{txnid}`。
- Webhook：表单编码，字段 `txnid/refno/status/message/amount/ccy/digest`，`digest = sha1(txnid:refno:status:message:merchantkey)`。Status 字母：S=Success, F=Fail, P=Pending, U=Unknown, R=Refund, K=Chargeback, V=Void, A=Auth, G=AwaitPay, E=Expired。

---

## 路由示意（payment-core 侧配置）

```yaml
routing:
  rules:
    # 电子钱包
    - { country: PH, payment_method: GCASH,    adapter: gcash }
    - { country: PH, payment_method: MAYA,     adapter: maya }
    - { country: PH, payment_method: GRABPAY,  adapter: grabpay }
    - { country: PH, payment_method: SHOPEEPAY,adapter: shopeepay }
    - { country: PH, payment_method: COINS_PH, adapter: coinsph }

    # 银行转账（<=50k 走 InstaPay 实时；>50k 走 PESONet 批量）
    - { country: PH, payment_method: BANK_TRANSFER, amount_max: 5000000,  adapter: instapay }
    - { country: PH, payment_method: BANK_TRANSFER, amount_min: 5000001, adapter: pesonet }

    # 直连银行（商户主动选择特定银行）
    - { country: PH, payment_method: BDO,       adapter: bdo }
    - { country: PH, payment_method: BPI,       adapter: bpi }
    - { country: PH, payment_method: METROBANK, adapter: metrobank }
    - { country: PH, payment_method: LANDBANK,  adapter: landbank }

    # PSP（兜底 / 商户通过聚合器上的）
    - { country: PH, payment_method: CARD,     adapter: paymongo }
    - { country: PH, payment_method: QRPH,     adapter: xendit }
    - { country: PH, payment_method: OTC,      adapter: dragonpay }

    # BNPL
    - { country: PH, payment_method: BNPL,     adapter: billease }
```

---

## 失败码归一

每家 adapter 在 response 路径上调 `channel.MapFailure(<name>, rawCode)`，映射到
order-core 约定的 7 个规范码：

| 规范码                 | 典型来源 |
|------------------------|---|
| `card_declined`        | Stripe `card_declined`、PayMongo `payment.failed.reason`、Dragonpay `F` with no subcode |
| `insufficient_funds`   | GCash `INSUFFICIENT_BALANCE`、Maya `INSUFFICIENT_FUND`、各银行 `NSF` |
| `risk_blocked`         | Maya `RISK_HIGH`、PayMongo `risk_decision=declined`、BillEase `credit_rejected` |
| `auth_failed`          | OTP/3DS 失败 / 银行密码错 |
| `expired`              | QR/OTP 过期、Maya `CHECKOUT_EXPIRED`、Dragonpay `E`、Xendit `EXPIRED` |
| `channel_unavailable`  | HTTP 5xx / OAuth 刷 token 失败 / 维护窗口 |
| `unknown`              | 兜底 |

---

## 重试与退避

HTTP 4xx（400/401/403/422）不重试，直接归一失败码返回 payment-core。
5xx / 超时 / 网络错误 → `acquirer_tx.state=failed` + `next_retry_at`，
`CallRetryWorker` 按 `60 * 2^n`（封顶 1h）重放，幂等表兜底不会重复下单。

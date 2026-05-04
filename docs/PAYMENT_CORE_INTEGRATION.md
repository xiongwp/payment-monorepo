# 接入 payment-core 的契约

本仓库 `order-core` 通过 `internal/channel.PaymentChannel` 接口调用下游。
真实部署里这个接口的实现就是一个 `payment-core` gRPC client。

> **三仓存储分层：**
> - `order-core`：分库分表，落订单 / PI / Refund / 通知 / 差错。
> - `payment-core`：**无状态、无 DB**，只做路由 + 调用 + 规范化三件事 ——
>   1. 按 `payment_method` / 国家 / 商户路由到具体的 `payment-channel`
>      adapter；
>   2. 调用 adapter 拿到原始结果；
>   3. 把渠道原始返回（含 webhook、错误码）规范化成 `order-core` 协议。
> - `payment-channel`：**也分库分表**，但落的是「渠道调用流水 +
>   幂等表」 —— 每次向第三方发出的 request、收到的 response、收到的
>   webhook 都要持久化原始报文（审计 / 对账 / 投诉举证用）；同时维护
>   `UNIQUE(adapter, idempotency_key)` 幂等表，重复请求直接回放首次响
>   应，**绝不向第三方重复下单**。分片键也用 `pi_id`，与 order-core
>   同分片号便于跨仓 join。
>
> 业务状态机、对账编排、退款编排、通知重试、差错处理 **全部留在
> `order-core`**。payment-core 不持久化任何数据，重启后无需恢复状态。

`payment-core` 内部按 `payment_method` 路由到具体 `payment-channel` adapter（GCash /
Maya / GrabPay / Coins.ph / InstaPay / PESONet / Stripe / Alipay / WeChat / ...）。

```
                    payment_method
order-core ────────────────────────▶ payment-core ──┬──▶ GCash adapter
                                                   ├──▶ Maya adapter
       ▲                                           ├──▶ GrabPay adapter
       │                                           ├──▶ Coins.ph adapter
       │                                           ├──▶ InstaPay adapter
       │                                           ├──▶ PESONet adapter
       └─── webhook ingest ────── payment-core ────┴──▶ ...
```

## 1. order-core 调 payment-core 的接口（必须实现）

`internal/channel/payment.go` 里定义了 6 个方法，payment-core 必须全部实现：

| 方法 | order-core 何时调 | 期望响应 |
|---|---|---|
| `Charge(req)` | `PaymentIntentService.Confirm` 之后 | succeeded / authorized / processing / requires_action / failed |
| `Capture(req)` | manual capture 流程的 `Capture` RPC | succeeded / failed |
| `Void(req)` | 撤销预授权 | succeeded / failed |
| `Refund(req)` | `RefundRetryWorker` + 用户主动退款 | succeeded / processing / failed |
| `Query(req)` | `ReconcileWorker` 对账 / 网络抖动恢复 | 真实状态快照 |
| `ParseWebhook(headers, body)` | `WebhookService.Ingest` 收到 payment-core 转发的回调 | 规范化 `WebhookEvent` |

## 2. 请求 / 响应字段对照（payment_method 路由）

`PaymentRequest.PaymentMethod` 是 payment-core 路由的依据。建议命名约定：

| payment_method | 渠道 | 说明 |
|---|---|---|
| `GCASH`        | GCash      | 菲律宾国民钱包，App redirect / scan QR |
| `MAYA`         | Maya       | Maya Wallet / Maya Bank |
| `GRABPAY`      | GrabPay    | 绑定 Grab 生态 |
| `COINS_PH`     | Coins.ph   | 小额 + remittance |
| `INSTAPAY`     | InstaPay   | 银行实时转账 |
| `PESONET`      | PESONet    | 银行批量转账（结算慢） |
| `ALIPAY`       | Alipay     | 中国支付宝（含 Alipay+ 全球） |
| `WECHAT_PAY`   | WeChat Pay | 中国微信支付 |
| `VISA` / `MASTERCARD` | 卡组织 | 走 Stripe / Adyen / 本地 acquirer |

## 3. 各电子钱包的 RequiredAction 映射

不同钱包对前端的"要让用户做什么"形态不同。在 `internal/channel/required_actions.go`
我们用强类型 helper 把这层规范化了，payment-core 的 adapter 直接构造对应的
`RequiredAction` 即可，无需关心 order-core 内部细节。

### GCash / Maya / GrabPay / Coins.ph (App 钱包)

```go
return &channel.PaymentResponse{
    ResultType:    channel.PaymentResultRequiresAction,
    ExternalRefNo: gcashRefNo,
    RequiredAction: channel.NewAppRedirectRequiredAction(channel.AppRedirectDetails{
        RedirectURL: "gcash://pay?token=xxx",   // GCash 的 deeplink
        Scheme:      "ios",                       // 或 "android" / "universal"
        ReturnURL:   req.ReturnURL,
        Extra: map[string]string{
            "merchant_name": "ACME",
        },
    }, time.Now().Add(15*time.Minute)),
}, nil
```

收银台拿到 `next_action.action_type == "app_redirect"` 后用 deeplink 唤起 App，
用户授权完成后回调到 `return_url`，前端再调 order-core 的查询或等 webhook。

### InstaPay / PESONet (银行转账)

通常返回扫码或显示账户信息，用户在网银端完成：

```go
return &channel.PaymentResponse{
    ResultType:    channel.PaymentResultRequiresAction,
    ExternalRefNo: instapayRefNo,
    RequiredAction: channel.NewQRCodeRequiredAction(channel.QRCodeDetails{
        CodeURL:        "https://qr.instapay.ph/...",
        ImageBase64:    pngBase64,
        ExpiresAt:      time.Now().Add(20*time.Minute),
        PollIntervalMs: 3000,
    }),
}, nil
```

### Maya / GCash 的 OTP 方式（绑卡 / 直接扣款）

```go
return &channel.PaymentResponse{
    ResultType:    channel.PaymentResultRequiresAction,
    ExternalRefNo: refNo,
    RequiredAction: channel.NewOTPRequiredAction(channel.OTPDetails{
        RecipientMasked: "+63 9** *** 1234",
        Channel:         "sms",
        Length:          6,
        ResendAfterSec:  60,
    }, "" /* code 由 Maya 自己保管 */, time.Now().Add(5*time.Minute)),
}, nil
```

注意：当 `code` 为空、`ChallengeID` 非空时，order-core 会走 `OTPVerifier.Verify`
反查路径，由 payment-core 调下游 Maya / GCash 验证。

### Stripe / Adyen / 银行卡 3DS

```go
return &channel.PaymentResponse{
    ResultType:    channel.PaymentResultRequiresAction,
    ExternalRefNo: pspRef,
    RequiredAction: channel.NewThreeDSRequiredAction(channel.ThreeDSDetails{
        RedirectURL: acsURL,
        SessionID:   dsTransID,
        ReturnURL:   req.ReturnURL,
    }, time.Now().Add(10*time.Minute)),
}, nil
```

## 4. Webhook 规范化（payment-core → order-core）

payment-core 收到任意渠道的异步通知后，做完签名校验，调
`order-core` 的 `WebhookService.Ingest`：

```
POST /order.v1.WebhookService/Ingest
{
  "direction":   "WEBHOOK_DIRECTION_CHANNEL",
  "channel_name": "payment-core",
  "headers":     {...},
  "body":        <raw bytes from channel>
}
```

`order-core` 再从 `PaymentChannelRegistry` 取 payment-core 客户端实例，
调 `ParseWebhook(headers, body)` 拿到规范化的 `WebhookEvent`：

```go
type WebhookEvent struct {
    EventID         string  // 渠道事件唯一 ID（用于幂等）
    EventType       string  // charge.succeeded / charge.failed /
                            // refund.succeeded / refund.failed /
                            // payment_intent.requires_action / ...
    PaymentIntentID string
    ChargeID        string
    RefundID        string
    ExternalRefNo   string
    Amount          int64
    Timestamp       time.Time
    RawPayload      map[string]string
}
```

`event_type` 必须使用以下规范化值，order-core 才能驱动状态机：

| event_type | order-core 行为 |
|---|---|
| `charge.succeeded` 或 `payment_intent.succeeded` | 推 PI → succeeded（晚到时走 `OnLateChargeSuccess` 自动补偿退款） |
| `charge.failed` 或 `payment_intent.failed` | PI → failed |
| `refund.succeeded` | Refund → succeeded |
| `refund.failed` | Refund → failed |
| `payment_intent.requires_action` | PI → requires_action（等 cashier 提交） |

## 5. 失败码对照（payment-core 内部映射，向 order-core 传规范码）

GCash / Maya / GrabPay / 银行各家原始码千差万别。建议 payment-core 层
**强制映射**到一份小集合，order-core 再据此做风控统计：

| 规范 failure_code | 含义 | 来源举例 |
|---|---|---|
| `card_declined` | 通用拒绝 | Stripe `card_declined`、Alipay `ACQ.SYSTEM_ERROR` |
| `insufficient_funds` | 余额不足 | GCash `INSUFFICIENT_BALANCE` |
| `risk_blocked` | 风控拦截 | Maya `RISK_HIGH`、Adyen `Refused (FraudPolicy)` |
| `auth_failed` | 鉴权失败 | OTP / 密码错误 / 3DS 未通过 |
| `expired` | 用户超时 | QR 码 / OTP 过期 |
| `channel_unavailable` | 渠道不可用 | 维护 / 限流 |
| `unknown` | 未分类 | 兜底 |

把这一层放在 payment-core，order-core 接收到统一码即可做指标 (`order_channel_charge_total{result="card_declined"}`) 和差错单分类。

## 6. 支付路由建议（payment-core 内部）

按业务侧需求 + 监管需求路由：

```yaml
# payment-core 配置示例（不在本仓库）
routing:
  - match: { country: PH, payment_method: GCASH }
    adapter: gcash_v2_partner       # 通过 GCash 的 Partner Portal
  - match: { country: PH, payment_method: MAYA }
    adapter: maya_paas
  - match: { country: PH, payment_method: GRABPAY }
    adapter: grabpay_payment_link
  - match: { country: PH, payment_method: COINS_PH }
    adapter: coins_ph_checkout
  - match: { country: PH, payment_method: INSTAPAY }
    adapter: bancnet_instapay
  - match: { country: PH, payment_method: PESONET }
    adapter: bancnet_pesonet
```

order-core 完全不感知这些细节——它只知道 `payment_method=GCASH` 应该让用户跳 App。

## 7. 本地联调清单

1. `payment-core` 没准备好时，order-core 用 `service.MockPaymentCoreChannel` 即可：
   - `payment_method` 包含 `GCASH/MAYA/GRABPAY` → 返回 App redirect
   - 包含 `BALANCE` → 返回 OTP（mock code = 123456）
   - 包含 `VISA` → 返回 3DS redirect
   - 包含 `FAIL` → 返回 failed
2. 模拟 webhook：
   ```
   grpc-client webhook -channel payment-core -event charge.succeeded -pi pi_xxx -charge ch_xxx
   ```
3. 退款流程：
   ```
   grpc-client refund -pi pi_xxx -reason requested_by_customer
   ```
4. 对账（关掉 webhook 让 ReconcileWorker 来收尾）：
   ```
   grpc-client create ... → confirm ... → 等 10 分钟 → 看日志 reconcile pass completed
   ```

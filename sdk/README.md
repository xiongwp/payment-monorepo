# SDKs

Official client SDKs for the Payment Platform API.

| Language | Path | Install |
|---|---|---|
| Node.js (≥18) | [`./node`](./node) | `npm install @payment-platform/sdk` |
| Python (≥3.8) | [`./python`](./python) | `pip install payment-platform-sdk` |
| Go (≥1.22) | [`./go`](./go) | `go get github.com/payment-platform/sdk-go` |
| Ruby (≥2.7) | [`./ruby`](./ruby) | `gem install payment_platform` |
| Java (≥11) | [`./java`](./java) | `mvn:com.payment_platform:payment-platform-sdk:0.1.0` |

## Common features (all SDKs)

- **Auto sandbox/live detection** — key prefix `pk_test_` 走 sandbox; `pk_live_` 走 prod
- **Automatic idempotency keys** — all POST/PUT/DELETE generate `Idempotency-Key` header
- **Retry with exponential backoff** — 5xx / 429 自动 3 次重试
- **Webhook signature verification** — `webhooks.constructEvent` (Stripe-style)
- **Zero dependencies** — 用各语言 stdlib

## Quickstart

### Node.js

```js
const Pay = require('@payment-platform/sdk');
const pay = new Pay({ apiKey: 'pk_test_...' });

const charge = await pay.charges.create({ amount: 1999, currency: 'USD', source: 'tok_xxx' });

// Express webhook handler
app.post('/webhook', express.raw({ type: 'application/json' }), (req, res) => {
  try {
    const event = pay.webhooks.constructEvent(
      req.body, req.headers['x-webhook-signature'], process.env.WEBHOOK_SECRET);
    if (event.type === 'charge.succeeded') { /* ... */ }
    res.json({ received: true });
  } catch (e) { res.status(400).send(e.message); }
});
```

### Python

```python
from payment_platform import Client

pay = Client(api_key="pk_test_...")
charge = pay.charges.create(amount=1999, currency="USD", source="tok_xxx")

# Flask webhook
@app.route("/webhook", methods=["POST"])
def webhook():
    try:
        event = pay.webhooks.construct_event(
            request.data, request.headers["X-Webhook-Signature"], os.environ["WEBHOOK_SECRET"])
        return {"received": True}
    except WebhookSignatureError as e:
        return str(e), 400
```

### Go

```go
import paymentsdk "github.com/payment-platform/sdk-go"

c := paymentsdk.New("pk_test_...")
charge, err := c.Charges.Create(ctx, &paymentsdk.ChargeParams{
    Amount: 1999, Currency: "USD", Source: "tok_xxx",
})

// HTTP webhook
http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
    body, _ := io.ReadAll(r.Body)
    event, err := c.Webhooks.ConstructEvent(body, r.Header.Get("X-Webhook-Signature"),
        os.Getenv("WEBHOOK_SECRET"), 300)
    if err != nil { http.Error(w, err.Error(), 400); return }
    _ = event
    w.WriteHeader(200)
})
```

## API version pinning

每个 SDK 内置 `API_VERSION = "2026-05-01"` 常量, 发请求时塞 `X-API-Version` header. server 端按 header 路由到对应版本 controller, 升级 breaking change 时商户原 SDK 还能用.

## 下一步

- Ruby SDK (常见 Stripe 用户来源)
- Java SDK (企业商户)
- 自动 codegen (openapi-generator + 我们的 OpenAPI specs)
- CLI tool (`payment-cli`) 用 SDK 包装

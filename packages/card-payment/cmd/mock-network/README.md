# mock-network — 5-in-1 卡组织 REST mock server

dev / e2e / sandbox 用，单进程模拟 Visa / Mastercard / JCB / AmEx / UnionPay
五家卡组织的 REST API。**生产严禁部署**（card-payment.assertProdSafety 已加 guard）。

## 启动

### 本地直接跑

```bash
# 默认 :9555 HTTP（dev）
go run ./cmd/mock-network

# 加 TLS（自签 cert）
openssl req -x509 -newkey rsa:2048 -keyout mock.key -out mock.crt -days 365 -nodes -subj "/CN=localhost"
go run ./cmd/mock-network --addr :9555 --cert mock.crt --key mock.key
```

### docker-compose

`payment-admin-web/stack/docker-compose.yml` 已加 `card-mock-network` 服务，
跟 `card-payment` 一起 `up`：

```bash
docker compose -f packages/payment-admin-web/stack/docker-compose.yml up -d card-mock-network card-payment
```

card-payment 自动指向 `http://card-mock-network:9555/<network>`。

## 路径

| 网络 | endpoint |
| --- | --- |
| Visa | `/visa/pts/v2/payments` (POST) + `/captures` `/refunds` `/voids` + `/tss/v2/transactions/{id}` |
| Mastercard | `/mastercard/api/rest/version/76/merchant/{m}/order/{id}/transaction/{txn}` (PUT/POST/GET) |
| JCB | `/jcb/api/v1/payments` + `/{id}/capture` `/refund` `/void` |
| AmEx | `/amex/payments/digital/v2/payments` + 同上 |
| UnionPay | `/unionpay/gateway/api/backTransReq.do` (form) + `/queryTrans.do` |
| Admin | `/healthz`, `/admin/txns` (列出全部内存 txn) |

## BIN 决定结果

5 家共 ~25 条规则；选 BIN 触发不同分支：

| BIN 前缀 | 状态 | 风险字段 |
| --- | --- | --- |
| `4242 / 5454 / 5555 / 3528 / 3589 / 3782 / 3714 / 62xx` | approved | AVS=Y CVV=M FraudScore=5..15 |
| `4000 / 5100 / 3500 / 3700 / 6225` | declined SOFT | INSUFFICIENT_FUNDS / 51 |
| `4001 / 5101 / 3501 / 3701 / 6226` | declined HARD | STOLEN_CARD / FRAUDULENT_TRANSACTION / 34 (FraudScore 90+) |
| `4002` | pending | PENDING_REVIEW |

其它 BIN：默认 approved + 中等 fraud_score (~25)。

## 故障注入

### 全局（启动 flag）

```bash
go run ./cmd/mock-network \
    --fault-rate 0.05  \  # 5% 概率随机 500
    --latency-ms 200   \  # 每个请求加 200ms 延迟
    --seed 12345          # rng seed (CI 复现)
```

### Per-request（HTTP header）

caller 在 Authorize 请求 header 里塞：

| Header | 值 | 行为 |
| --- | --- | --- |
| `X-Mock-Fault` | `timeout` | sleep 60s 模拟卡组织 hang |
| `X-Mock-Fault` | `500` | 直接 500 + 空 body |
| `X-Mock-Fault` | `429` | 直接 429 + Retry-After: 1 |
| `X-Mock-Fault` | `tls` | hijack 关连接（模拟 TLS 错） |
| `X-Mock-Latency-Ms` | `5000` | 该请求加 5s 延迟（最大 60s） |

测试 card-payment 的重试 / 熔断 / 超时路径用。

## 异步 webhook 回调

approved 之后 mock 会延迟 N 秒（默认 3s）主动 POST 一条
`{event: "transaction.settled", network_ref_no, amount, ...}` 给商户回调地址。

### 设默认回调

```bash
go run ./cmd/mock-network \
    --webhook-url http://card-payment:9444/_callback \
    --webhook-delay-ms 2000
```

### Per-request 覆盖

caller 在 header 里塞 `X-Mock-Webhook-URL: http://my-handler/cb`，本笔交易用这个。

## 状态机

每笔交易在 mock 内存 store 里：

```
Authorize → approved/declined/pending
   ↓
Capture  → captured  (approved → captured)
Refund   → refunded  (approved/captured → refunded)
Void     → voided    (任何状态 → voided)
Query    → 当前 status
```

**无持久化**：mock 进程重启后所有交易状态丢失。dev/e2e 跑完即弃。

## 调试

```bash
# 健康检查
curl http://localhost:9555/healthz

# 列所有 mock txn
curl http://localhost:9555/admin/txns | jq

# 跑 happy path Visa
curl -X POST http://localhost:9555/visa/pts/v2/payments \
  -H "Content-Type: application/json" \
  -d '{"clientReferenceInformation":{"code":"test123"},
       "orderInformation":{"amountDetails":{"totalAmount":"1.00","currency":"USD"}},
       "paymentInformation":{"card":{"number":"4242424242424242","expirationMonth":"12","expirationYear":"2030"}}}'

# 跑 hard decline
curl ... -d '{"...":"...","number":"4001000000000001",...}'

# 跑 timeout 注入
curl ... -H "X-Mock-Fault: timeout" ...
```

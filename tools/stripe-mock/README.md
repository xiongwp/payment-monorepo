# Stripe / Adyen Mock Server

为 e2e 测试 + dev 环境提供"真实卡组应答"的 HTTP/gRPC mock。
不依赖外部网络,SLO 测试可复现。

## 部署

```bash
docker run --rm -p 12111:12111 \
  -v $(pwd)/scenarios:/scenarios \
  ghcr.io/example/stripe-mock:latest
```

## 场景

| Scenario | Request 特征 | Response |
|----------|-------------|----------|
| `approved` | 默认 | 200, status=succeeded |
| `declined_insufficient_funds` | amount > 1,000,000 cents | 200, status=declined, decline_code=insufficient_funds |
| `declined_do_not_honor` | last4 = 0002 | 200, decline_code=do_not_honor |
| `network_timeout` | header `X-Mock-Scenario: timeout` | hang 60s |
| `network_5xx` | header `X-Mock-Scenario: 5xx` | 503 |
| `partial_capture` | amount = 99, capture_amount = 50 | succeeded but captured < requested |
| `3ds_challenge_required` | amount in [50, 100] | requires_action with 3DS challenge URL |

## 集成

K6 / e2e 测试用 `Authorization: Bearer mock_$SCENARIO` header 触发对应场景。
payment-channel 在 dev 环境通过 env `CARD_PAYMENT_ENDPOINT=http://stripe-mock:12111` 接入。

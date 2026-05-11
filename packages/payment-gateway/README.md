# payment-gateway

统一支付网关 + 智能路由 + 卡 PAN 令牌化。商户接一个 SDK，gateway 决定走哪个通道。

## 能力

- **routing/** 多目标加权路由：success_rate / fee / latency / merchant_pref / canary
  - EWMA 实时反馈通道健康 + p99 latency 跟踪
  - filter by product / currency / region / BIN range / amount range
  - primary + 3 个 fallback 故障转移
- **tokenize/** PCI scope 收缩
  - Luhn 校验 + BIN→brand 识别
  - KMS envelope 加密 PAN（CVV 不存）
  - one_time (15min ttl) / reusable (1y)
  - audit log 每次 detokenize 调用（who/when/why）

## 启动

```bash
docker build -t payment-gateway:local .
docker run --rm -p 8080:8080 payment-gateway:local
```

## API

```bash
# 1. tokenize 卡
curl -X POST localhost:8080/api/v1/tokens -d '{
  "card": {"pan":"4111111111111111","expiry_mm":12,"expiry_yy":30,"cvv":"123","holder_name":"Alice Smith"},
  "type": "one_time"
}'

# 2. 智能路由决策（不真扣款，调试用）
curl -X POST localhost:8080/api/v1/route -d '{
  "merchant_id":"mer_test","amount_minor":10000,"currency":"PHP","region":"PH",
  "product":"card_charge","card_bin":"411111"
}'

# 3. 统一 charge — token + route + 通道（stub）
curl -X POST localhost:8080/api/v1/charge -d '{
  "token_id":"tok_xxx","merchant_id":"mer_test",
  "amount_minor":10000,"currency":"PHP","region":"PH","product":"card_charge"
}'
```

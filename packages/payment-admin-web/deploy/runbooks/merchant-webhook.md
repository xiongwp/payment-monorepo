# merchant-webhook Runbook

## Service Overview
- **Purpose**: 商户出站事件投递 (HMAC + 10次指数退避 + DLQ)
- **SLO**: 99% 1h delivery rate / DLQ 任一 entry alert
- **Port**: :18093

## Dependencies
↑ refund / dispute / billing / payment-core (业务发事件)
↓ 商户自己的 webhook URL
Effect when down: 商户收不到通知 — 系统不一致风险

## Common Alerts

### `WebhookDLQEntry` — 任一 delivery 进 DLQ
含义：3 天 + 10 次重试都失败 = 商户 webhook 长期挂了
1. 看 endpoint URL + last_error
2. 联系商户客服确认 webhook 是否还有效
3. 可选: 关掉对应 endpoint (active=false)
4. DLQ entry 保留 30 天供 replay

### `WebhookLagHigh` — pending → delivered 滞后 > 30s
正常 1s 内一轮。慢说明：
- worker 卡 (kubectl rollout restart)
- 商户 webhook 全慢 (5s timeout 每条)
- HPA 没 scale 起来

## Common Ops
```bash
# 列商户 endpoint
curl :18093/api/v1/endpoints?merchant_id=mer_xxx

# Replay 某 DLQ entry (TODO impl)
curl -X POST :18093/api/v1/dlq/123/replay
```

# refund-engine Runbook

## Service Overview
- **Purpose**: 独立退款引擎 — 状态机 + 4-eyes + 跨服务通知（webhook/billing）
- **SLO**: 99.9% / refund.requested → completed < 2min 99%
- **Port**: :18094

## Dependencies
↑ payment-core / dispute-service / ops admin
↓ merchant-webhook (NotifyMerchant) / billing-system (NotifyBilling) / 通道 SDK
Effect when down: 退款全部卡住，pending → 商户投诉激增

## Common Alerts

### `RefundCompletionTimeHigh` — P99 > 5min
1. 查 cron `Submit` 是否在跑（每 5s 一次）
2. 查通道 SDK 慢: `kubectl logs ... | grep "channel error"`
3. webhook 投递失败堆积? 查 merchant-webhook DLQ count
4. **Mitigation**: scale-out + 优先 process 大额 refund（按 amount desc 排）

### `RefundExcessAlert` (来自 reconplatform catalog)
某 charge 累计 refund > 原 charge — 资金事故，立即:
1. 锁定该 refund_id：`UPDATE refund SET status='void' WHERE id=...`
2. 查 audit log 找操作者
3. 通知财务 + page 安全

## Common Ops
```bash
# ops 复核大额 refund
curl -X POST :18094/api/v1/refunds/123/approve -H "X-Admin-User: ops@x"

# 拒绝（疑似欺诈）
curl -X POST :18094/api/v1/refunds/123/reject -d '{"reason":"...");
```

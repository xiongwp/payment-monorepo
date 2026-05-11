# dispute-service Runbook

## Service Overview
- **Purpose**: 信用卡争议状态机 + 商户证据收集 + 卡组裁定 ingest
- **SLO**: 0 disputes 过 deadline（合规硬指标）
- **Port**: :18092

## Dependencies
↑ 卡组 webhook (Visa/MC) → /api/v1/disputes/webhook
↓ merchant-webhook (商户通知) / clearing-settlement (reserve hold)
Effect when down: 收不到 chargeback 通知 → 自动 lost → 资金事故 + 卡组罚款

## Common Alerts

### `DisputeOverdueAny` — 任一 dispute 过 deadline 未 response
1. **HIGH PRIORITY** — 直接看 dispute id + merchant_id
2. 联系商户客服催提交证据
3. **必要时手工提交残缺证据** 比超期 lost 强（卡组允许部分证据）
4. 24h 内 finance/legal 复盘

### `NetworkWebhookSilent` — 24h 没收到任何 chargeback 通知
1. 正常状态：每周 1-2 笔（视交易量）
2. 24h 全静默 = 可能卡组 webhook 配置坏了
3. 联系卡组 acquirer ops，要求重发最近 24h notification

## Common Ops
```bash
# ops 帮商户提交证据（紧急救场）
curl -X POST :18092/api/v1/disputes/123/evidence -d '{"type":"receipt","file_url":"..."}'
curl -X POST :18092/api/v1/disputes/123/finalize -H "X-Reviewer: ops@x"
```

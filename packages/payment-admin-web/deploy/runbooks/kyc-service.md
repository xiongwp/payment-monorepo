# kyc-service Runbook

## Service Overview
- **Purpose**: 商户 KYB onboarding + 持续 PEP/sanctions monitoring
- **Owner**: compliance team
- **SLO**: 70%+ low-risk 自动 approve
- **Port**: :18095

## Dependencies
↑ payment-admin-web (商户后台提交)
↓ Sumsub/Veriff API (文件 verify) / Refinitiv (PEP)
Effect when down: 新商户 onboard 卡住（已激活商户不影响）

## Common Alerts

### `KYCAutoApproveRateLow` — < 50%
某段时间多数被判 high risk → 可能 sanctions 表更新太严 / 商户质量下降
1. 看最近 reject 案例分布
2. 跟 compliance 复核是否 false positive

### `KYCSanctionsHit` — 任一商户命中制裁名单
**P0** — 立即:
1. 暂停商户交易 (UPDATE merchant SET status='blocked')
2. 通知 compliance + 法务
3. 24h 内提交 SAR (Suspicious Activity Report)

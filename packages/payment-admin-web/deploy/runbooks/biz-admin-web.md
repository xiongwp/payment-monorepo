# biz-admin-web Runbook

## Service Overview
- **Purpose**: 8 服务统一控制台 + e2e demo
- **SLO**: 99% / 慢就慢，不影响业务
- **Port**: :18099

## Dependencies
↓ 全部 8 业务服务 (reverse proxy)

## Common Alerts

### `AdminWebUnhealthy`
ops 看不到东西 — 但不影响业务流量。
1. kubectl rollout restart deployment/biz-admin-web
2. /health/all 看哪个 backend 挂了

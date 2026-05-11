# audit-log Runbook

## Service Overview
- **Purpose**: 全局审计哈希链 — 防篡改
- **Owner**: security team
- **SLO**: hash chain 0 中断
- **Port**: :18096

## Common Alerts

### `AuditChainTampered` — chain verify 失败
**P0 CRITICAL** — 篡改信号:
1. 立即冻结所有 admin web 写权限 (block POST)
2. snapshot 当前数据库 (PITR)
3. 调查 bad_index 附近 entry，找篡改时间窗
4. **24h 内通报法务 + 监管 (如果是上市公司)**
5. 启用 incident response IRT

## Common Ops
```bash
# 校验 chain
curl :18096/api/v1/audit/verify

# 月底导出 CSV → WORM
curl :18096/api/v1/audit/export?format=csv > audit-$(date +%Y%m).csv
aws s3 cp audit-*.csv s3://payment-audit-worm/ --storage-class GLACIER
```

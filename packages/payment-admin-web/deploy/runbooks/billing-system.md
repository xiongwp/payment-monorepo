# billing-system Runbook

## Service Overview
- **Purpose**: 商户计费 + 月度账单聚合
- **Owner**: payments team (@payments-oncall)
- **Repo**: `packages/billing-system/`
- **Endpoints**: `:18090` (k8s `billing-system.payment.svc.cluster.local:8080`)
- **SLO**:
  - Availability 99.95% / 5xx < 0.05% / P99 < 500ms

## Dependencies

```
↑ Upstream
  - order-core (HTTP POST /api/v1/fee/events 喂 charge 事件)
  - payment-channel (同上)
  - refund-engine (HTTP，refund 完成调 NotifyBilling)

↓ Downstream
  - billing_db MySQL (writes + reads)
  - audit-log (logs ops 操作)

Effect when down:
  - 商户当月账单延迟（账单聚合 cron 不跑）
  - refund fee_event 没 record → fee 对账失败
  - 不阻塞 payment 主流程（fee_calc 端点 dry-run 不影响 charge）
```

## Common Alerts

### `BillingAvailabilityDegraded` — 5xx > 0.05%

1. `kubectl logs -n payment -l app=billing-system --tail=200 | grep ERROR`
2. 看 MySQL 是否慢:
   ```bash
   kubectl exec -n payment billing-mysql-0 -- \
       mysql -uroot -p$PWD -e "SHOW PROCESSLIST" | grep -v Sleep
   ```
3. 检查 fee_event 表大小:
   ```sql
   SELECT TABLE_NAME, TABLE_ROWS, DATA_LENGTH/1024/1024 AS mb
   FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'billing_db';
   ```
4. **Mitigation**:
   - `kubectl rollout restart deployment/billing-system`（重启）
   - 看 HPA 是否 scale 上限 → 临时提高 maxReplicas

### `BillingAggregatorStuck` — 日切 cron 02:00 后无 statement 产出

1. 看 logger 是否报错:
   `kubectl logs --since 4h -n payment -l app=billing-system | grep aggregator`
2. 手动触发:
   ```bash
   curl -X POST http://billing-system:18090/api/v1/aggregate \
        -d '{"from":"2026-05-09T00:00:00Z","to":"2026-05-10T00:00:00Z","final":false}'
   ```
3. 检查 fee_event 表是否有 pending 待结算:
   ```sql
   SELECT merchant_id, COUNT(*) FROM fee_event WHERE status='pending' GROUP BY merchant_id;
   ```

### `BillingFeeMismatch` (来自 reconplatform catalog)

reconplatform 检测到 fee_event 跟 charge 算的不一致。看 reconplatform admin
diff 详情，按 trace_id 跳 Jaeger 看完整链路。

## Common Operations

### 增加新 fee rule（生产硬上限审批）

```bash
curl -X POST http://billing-system:18090/api/v1/rules \
  -H "X-API-Key: $OPS_KEY" \
  -d '{
    "name": "Premium-Card-EU",
    "priority": 100,
    "region": "EU",
    "card_bin_range": "400000-499999",
    "percent_bps": 290,
    "fixed_minor": 30,
    "active": true,
    "effective_from": "2026-05-15T00:00:00Z"
  }'
```

⚠️ 大额商户 rule 必须经过财务复核。本端点应该有 audit_log 留痕。

### 手工 statement 重生成（误删后救场）

```bash
curl -X POST http://billing-system:18090/api/v1/aggregate \
  -d '{"from":"2026-04-01T00:00:00Z","to":"2026-05-01T00:00:00Z","final":true}' \
  -H "X-API-Key: $OPS_KEY"
```

## Disaster Recovery

| 场景 | 影响 | 恢复步骤 | RTO |
|---|---|---|---|
| MySQL master crash | 写不进 fee_event | 切换 read replica → master | 5min |
| billing-system pod 全挂 | 商户账单查询 503 | HPA 自动伸缩 / `kubectl rollout` | 2min |
| 数据库整库丢失 | 财务事故 | PITR 恢复 + 重跑 aggregator | 4h |

## Debugging

```bash
# 本地连 prod (要 jumpbox)
kubectl port-forward -n payment svc/billing-system 18090:8080

# 看 fee event 流水
mysql -h127.0.0.1 -P3406 -ubilling_app -p billing_db \
  -e "SELECT id, merchant_id, event_type, fee_minor, currency, occurred_at
      FROM fee_event ORDER BY id DESC LIMIT 50"

# 强制 fee_event 重算 (开发 only - 不要在生产用)
curl -X POST :18090/api/v1/fee/events \
  -H "X-Internal-Token: $TOKEN" \
  -d '{...}'
```

## Postmortem Template

任何 P0/P1 触发后填写：`postmortem-template.md`

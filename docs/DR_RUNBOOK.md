# Disaster Recovery (DR) Runbook + 演练 SOP

平台部署在 multi-region (主 us-east-1 + 备 us-west-2). 主 region 整挂时切流到备 region.

## RTO / RPO 目标

| 维度 | 目标 | 当前 |
|---|---|---|
| **RTO** (Recovery Time Objective) | ≤ 1h | TBD (季度演练衡量) |
| **RPO** (Recovery Point Objective) | ≤ 5min (MySQL binlog lag) | 监控 `mysql_replication_lag_seconds` |

合规要求 (PCI-DSS / SOC 2): RTO ≤ 24h, RPO ≤ 1h. 我们目标更严.

## 灾难场景定义

| 等级 | 描述 | 触发标准 |
|---|---|---|
| **L1 — 单 AZ 挂** | 1 个 availability zone 失联 | K8s scheduler 自动 reschedule, 无需人 |
| **L2 — region 部分服务挂** | 1 个 region 内某 service 全死 | service-level 切流 |
| **L3 — region 整挂** | 1 整个 region 网络/电力/灾害失联 | **DR plan 启动** |
| **L4 — 多 region 并发挂** | 黑天鹅 (常见原因: 全球依赖 cloudflare/DNS 挂) | 全栈 readonly, 通知监管 |

本 runbook 聚焦 **L3** — region 整挂.

## 切换前提

- DNS / Route53 active-active 模式, weight 当前主 region 100% / 备 region 0%
- MySQL 跨 region 同步 (binlog → 备 region, 5min lag SLA)
- S3 跨 region replication (跨年税表 / 加密 PAN backup 等)
- Kafka 跨 region MirrorMaker 2 (outbox 事件不丢)
- KMS keys 备 region 同步 (旧 KEK / 新 KEK 都有)

## 切换流程 (L3 触发)

### Phase 1: 决策 (T-0 ~ T+5min)

1. PagerDuty incident 触发 (主 region 多服务 5xx > 50% > 5min)
2. on-call 拉群 (ops + SRE + 业务 lead + CTO)
3. 确认主 region 真挂 (非 monitoring 假阳性):
   - `kubectl --context=us-east-1 get pods -A` 不通
   - cloud provider status page red
   - 用户报障 spike

4. CTO / SRE lead 拍板执行 DR (双人复核 — 通过 `approval-service`)

```bash
curl -X POST http://approval-service:8092/v1/actions -d '{
  "type": "dr_failover",
  "resource": "region:us-east-1",
  "requester": "sre_alice",
  "required_approvals": 2,
  "payload": {"target_region": "us-west-2", "reason": "us-east-1 down 5min"}
}'
```

### Phase 2: 切流 (T+5 ~ T+15min)

```bash
# 1) 提升备 MySQL replica 为 master (跨 region 异步同步)
./scripts/dr-failover.sh promote-mysql us-west-2

# 2) Kafka MirrorMaker 切方向 (us-east-1 → us-west-2 改为 reverse)
./scripts/dr-failover.sh promote-kafka us-west-2

# 3) DNS 切流 (Route53 weight 0/100 → 100/0)
./scripts/dr-failover.sh switch-dns us-west-2
# 等 DNS TTL (我们设 60s)

# 4) K8s deploy autoscale 备 region 副本数
kubectl --context=us-west-2 -n payment scale deploy --all --replicas=3 -l app.kubernetes.io/part-of=payment-platform
```

### Phase 3: 验证 (T+15 ~ T+45min)

```bash
./scripts/dr-failover.sh verify us-west-2
# 检查:
#   ✓ DNS 解析到备 region
#   ✓ 商户 sandbox /v1/charges 200
#   ✓ KYB /v1/screen 200
#   ✓ payout /v1/payouts query 200
#   ✓ webhook 推送在跑 (deliveries 表有 last_5min)
#   ✓ accounting trial balance == 0 (数据没丢)
#   ✓ outbox publisher 在推 (rate >0)
```

### Phase 4: 复盘 (T+1 ~ T+24h)

- 主 region 恢复后**不立即切回**, 备 region 跑 24h 验稳定性
- 主 region 数据回追 (备 region binlog → 主)
- 切回主 region (反向 DR 切流)
- Post-mortem (3 天内, 写到 wiki)

### Phase 5: 通知监管 (如适用)

| 监管 | 触发 | 时限 |
|---|---|---|
| PCI-DSS 持卡人数据泄漏 | RPO 数据丢失 | 立即 |
| GDPR breach | EU 用户数据丢失/泄漏 | 72h |
| SEC | 影响 > 4h 业务 (上市公司) | 4 business days |
| 各国 reg | 资金安全失稳 | 24h |

## 演练 (季度强制)

每季度第 1 个周六凌晨 03:00-05:00 跑一次完整 DR drill (低流量窗口):

```bash
./scripts/dr-drill.sh
```

drill 跟真切流的区别:
- **不**真切 DNS (避免商户感知); 用 staging 流量
- **不**降主 region (仅备 region 跑负载测试)
- **测**所有路径都能 work: MySQL promote / Kafka switch / K8s scale
- **测** RTO / RPO 测量

drill 完产 `dr-drill-{date}.json` 报告 (SRE wiki 归档).

## 自动化脚本接口

`scripts/dr-failover.sh`:
- `promote-mysql <region>` — 提升备 MySQL 为 master
- `promote-kafka <region>` — Kafka MM 切方向
- `switch-dns <region>` — Route53 weight
- `verify <region>` — 跑健康检查
- `rollback` — 反向切回

`scripts/dr-drill.sh`:
- 自动跑 phase 2-3, 测 RTO/RPO
- 不影响生产流量
- 出 markdown 报告

## SLO + 告警

```yaml
# Prometheus rules
- alert: MySQLReplicationLagOverRPO
  expr: mysql_replication_lag_seconds > 300  # 5min RPO
  for: 2m
  labels: { severity: critical, sla: RPO-VIOLATION }
  annotations:
    summary: "MySQL replication lag {{ $value }}s exceeds 5min RPO target"

- alert: KafkaMirrorMakerLag
  expr: kafka_consumer_lag{group="mirror-maker-cross-region"} > 1000
  for: 2m
  labels: { severity: critical }

- alert: DRDrillOverdue
  expr: time() - dr_drill_last_success_timestamp > 90 * 86400
  labels: { severity: warning }
  annotations:
    summary: "DR drill 上次 success 距今 > 90 天"
```

## 检查清单 (drill / 真切前)

- [ ] 备 region 健康 (`kubectl --context=us-west-2 get pods -A | grep -v Running | wc -l` = 0)
- [ ] MySQL 跨 region replication lag < 60s
- [ ] Kafka MirrorMaker lag < 1000 msgs
- [ ] S3 replication 状态正常
- [ ] KMS keys 都同步到备 region
- [ ] approval-service 至少 2 个 approver 在线
- [ ] PagerDuty 接通
- [ ] Status page 准备好公告草稿

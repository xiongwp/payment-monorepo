# Disaster Recovery Plan

## RTO / RPO 目标 (生产)

| 场景 | 检测 | RTO | RPO | 恢复机制 |
|---|---|---|---|---|
| 单 pod crash | 5s | 30s | 0 | k8s restart + readiness 切流量 |
| 整 node 故障 | 1min | 5min | 0 | k8s 重新调度 pod, 副本无感 |
| 单 AZ 故障 (3-AZ deploy) | 1min | 15min | 0 | 跨 AZ replication, Kafka RF=3 |
| 整 region 故障 | 5min | 4h | 1h | S3 备份 + binlog ship 到 DR region |
| 数据库主库 OOM | 30s | 1min | 0 | semi-sync replica auto-promote |
| KMS 短暂不可用 (15min) | 1min | 0 (无感) | 0 | token cache 续 1h, 异步重试 envelope |
| Redis 集群挂 | 30s | 5min | <1min | Sentinel failover, 业务路径都有 DB fallback |
| 渠道 API 大面积失败 | 2min | 15min | 0 | Bulkhead + circuit breaker, 降级到队列 |
| 支付路由系统瘫 | 1min | 5min | 0 | api-gateway 走 cache 跳过 routing, 转到默认渠道 |

## 备份策略

- **DB 全量**: 每日 02:00 UTC, S3 STANDARD_IA, 40 份 (7d + 4w + 12m)
- **DB 增量**: binlog 实时 ship 到 DR region, RPO = 5s
- **KMS 主密钥**: 双人物理保管 + HSM 离线备份
- **Kafka topic**: 跨 3 broker 复制, retention 7d
- **Redis**: AOF + 每小时 RDB

## 演练

- **季度**: `restore-drill.sh` (cronjob, Jan/Apr/Jul/Oct)
- **半年**: 全 region failover GameDay (人工)
- **年**: PCI DSS QSA audit

## 责任 (RACI)

| 阶段 | Responsible | Accountable | Consulted | Informed |
|---|---|---|---|---|
| 检测 | SRE on-call | SRE lead | — | engineering |
| 升级决策 | SRE on-call | SRE lead | service owner | exec |
| 数据恢复 | DBA | SRE lead | service owner | — |
| 公告 | comms lead | CEO | legal | customers |
| Postmortem | service owner | SRE lead | all responders | engineering |

## 触发条件 → 行动 matrix

| 触发 | 自动行动 | 人工行动 |
|---|---|---|
| 任何 sev1 page | PagerDuty 通知 oncall (1min) | 接电话 + ack |
| 5 分钟没 ack | escalate 二线 | 二线 ack |
| 10 分钟无人 ack | escalate manager | 经理介入 |
| Critical fund safety alert | freeze charge endpoint (feature flag) | 财务介入 |
| DB primary OOM | semi-sync replica promote (1min) | 验证 promote 成功 |
| Region failover triggered | DNS 切到 DR + 通知 customers | 人工确认数据一致 |

## 演练历史

| 日期 | 类型 | 通过 | 备注 |
|---|---|---|---|
| (待填) | restore drill | — | — |

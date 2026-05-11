# Multi-Region Deployment

Active-Passive 跨 region 部署 — primary 跑全部生产流量, secondary 实时同步 + 失效后 4h 内顶上。

## 拓扑

```
                            ┌──── Route 53 / Cloudflare ────┐
                            │  payment.example.com          │
                            │  weighted: 100% → us-east-1   │
                            │            0% → eu-west-1     │
                            │  health check → 失败 swap     │
                            └────────┬──────────────────────┘
                                     │
                ┌────────────────────┴───────────────────────┐
                │                                            │
        ┌───────▼──────────┐                    ┌────────────▼─────────┐
        │  us-east-1        │                    │  eu-west-1            │
        │  (PRIMARY)        │   binlog ship      │  (DR PASSIVE)         │
        │  ────────────     │ ◄────────────────► │  ────────────────     │
        │  k8s cluster      │                    │  k8s cluster (idle)   │
        │  RDS primary      │  asynchronous repl │  RDS read replica     │
        │  S3 (active)      │   cross-region     │  S3 (replica)         │
        │  Redis primary    │                    │  Redis read replica   │
        │  Kafka MM2        │ ◄──── mirror ────► │  Kafka MM2 mirror     │
        └───────────────────┘                    └───────────────────────┘
```

## 数据复制策略

| 数据 | 机制 | RPO 目标 | 验证 |
|---|---|---|---|
| MySQL | semi-sync replication + cross-region async (binlog ship) | < 60s | `pt-table-checksum` 每日 |
| Kafka | MirrorMaker 2.0, 双向 | < 30s | `kafka-consumer-groups` lag |
| Redis | Redis Enterprise CRDB / native AOF ship | < 5min | `INFO replication` |
| S3 / 对象存储 | S3 Cross-Region Replication (CRR) | < 15min (99.99% < 1min) | inventory diff 每小时 |
| KMS 密钥 | AWS KMS Multi-Region Keys | 0 (同步) | failover smoke test |
| 配置 (etcd) | etcd cluster 跨 region (3 + 2 节点) | 0 | `etcdctl member list` |

## RTO 目标

- **手动 failover** (常规): 30min (灰度 DNS 切, 看流量 5min, 全切)
- **自动 failover** (health check 触发): 5min (Route53 健康检查 30s × 3 失败 + TTL 60s + 容器 ready 2min)

## 部署 manifest

```bash
# Region: primary (us-east-1)
kubectl --context=prod-use1 apply -f deploy/multi-region/region-primary/
kubectl --context=prod-use1 apply -f packages/oauth2-server/deploy/k8s/oauth2-server.yaml

# Region: secondary (eu-west-1) — 副本数=0 待机
kubectl --context=prod-euw1 apply -f deploy/multi-region/region-secondary/
```

## DNS Failover

```bash
# 半自动 failover (有 ack)
./deploy/multi-region/failover.sh --from us-east-1 --to eu-west-1 --reason "primary outage"

# 自动 health check 触发 — Route53 Health Check + Failover routing policy
# 见 deploy/multi-region/route53-failover.tf
```

## 切换 SOP

1. **Detect** — Alertmanager: 任何 region 整体不可达 5min → sev1 + page
2. **Decide** — On-call lead 决策 (1min): 是 region 真挂还是误报?
3. **Verify** — 跑 `failover-precheck.sh`: 确认 DR region 健康 + 副本 lag < RPO
4. **Switch DNS** — `failover.sh` 改 Route53 weighted record (100% → DR)
5. **Scale up DR** — `kubectl --context=prod-euw1 scale deployment ... --replicas=N`
6. **Promote DB** — DR MySQL replica 升 primary (确认 binlog 追上)
7. **Verify** — synthetic probe 5min 看流量正常 + 业务 invariant 检查
8. **Comms** — 公告 status page + 通知 customers
9. **Postmortem** — 写 RFC, 包括 RTO/RPO 实际达成 vs 目标

## 一些已知约束

- **Kafka MM2 双向**: 防 message loop 必须配 `replication.policy.separator` (default `.`)
- **MySQL 跨 region**: async replication 给了 RPO 60s 上限; sync 太慢不可行
- **Stripe / Adyen 等渠道 webhook**: 必须在 DNS failover 同时改 webhook URL (调用方主动重新订)
- **KMS Multi-region keys**: 必须用 alias 而非 key ARN (alias 跟 region 走)
- **OAuth2 JWKS**: 两个 region 共享 RSA private key (sealed-secret + 跨 region replicate); 或用同一个 oauth2-server (DR region 是只读 cache, 见 `OAUTH2_MULTI_REGION.md`)

## 季度 Failover Drill

每季第一周六 02:00 UTC 跑:
1. 通知 customers (准备维护窗口 1h)
2. `failover.sh primary → DR`
3. 业务跑 30min 在 DR
4. `failover.sh DR → primary`
5. Postmortem

跑过的 drill 记入 [`docs/DR_PLAN.md`](../../docs/DR_PLAN.md).

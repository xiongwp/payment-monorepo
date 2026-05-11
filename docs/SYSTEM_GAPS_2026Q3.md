# 全栈能力盘点 — 2026Q3

针对 11+ 服务的支付平台，从 **监控 / 可运维 / 可扩展 / 灾备 / 合规** 5 维度做差距评估。
本文档列已有能力 (✅) + 待补 (🚧) + 优先级 (P0~P2)。

---

## 一、监控 (Monitoring)

| 维度 | 现状 | 状态 | 优先级 |
|---|---|---|---|
| Metrics | Prometheus + alertmanager + 多服务 11 个 exporter | ✅ 完整 | — |
| Tracing | OTel + Jaeger; trace_id 透传 | ✅ 完整 | — |
| Logging — 单服务 | 各服务 zap stdout | ✅ | — |
| **集中日志** | 11 个 docker logs / kubectl logs 翻 | 🚧 缺 Loki/ELK | **P0** |
| Alerting | Alertmanager → Slack | ✅ 基础 | — |
| **Alert routing** | 1 个 channel 接收全部, 没分 sev | 🚧 缺 routing tree | P1 |
| **Synthetic monitoring** | 没 SLA 探测，靠 4xx/5xx 反推 | 🚧 缺 blackbox probes | **P0** |
| Business metrics | 部分 (oauth2 token, refund count) | 🚧 缺 GMV / 渠道成功率 dashboard | P1 |
| **Error budget tracking** | SLO 写了没人盯 | 🚧 缺 burn-rate alert | P1 |
| RUM (前端) | 没 | 🚧 缺 | P2 |

---

## 二、可运维 (Operability)

| 维度 | 现状 | 状态 | 优先级 |
|---|---|---|---|
| Config 集中 | config-center (etcd-backed) | ✅ | — |
| **Feature flags** | 没专门 SDK, 改 config 要重启 | 🚧 缺 hot-reload flag SDK | **P0** |
| Service discovery | etcd | ✅ | — |
| Secrets | KMS envelope + sealed-secrets | ✅ | — |
| Runbook | docs/runbooks/RUNBOOK.md (单文件) | 🟡 部分 | P1 (拆分 per-service) |
| Health check | /healthz 全服务 | ✅ | — |
| **Service dependency map** | 文档手画过时 | 🚧 缺 OTel-derived graph | P1 |
| Canary deploy | k8s rollout 支持 | ✅ | — |
| Blue/green | 没 | 🚧 | P2 |
| Rollback automation | k8s rollback 手动 | 🟡 | P2 |
| On-call rotation | 没集成 (没 PagerDuty/OpsGenie) | 🚧 | P1 |
| Postmortem template | 已有 | ✅ | — |

---

## 三、可扩展 (Scalability)

| 维度 | 现状 | 状态 | 优先级 |
|---|---|---|---|
| HPA | k8s HPA 全服务 | ✅ | — |
| DB sharding | RouterV2 10 shard, 支持 1000 | ✅ | — |
| Read replica | partial (没主从读写分离) | 🟡 | P2 |
| Cache | Redis 7d 热, ClickHouse 冷 (reconplatform) | ✅ partial | — |
| Message queue | Kafka (audit, ledger, webhook) | ✅ | — |
| Connection pool | 各服务 DSN 配 max=20 默认 | 🟡 | P2 (调优) |
| **Async job queue** | 没专门, 用 Kafka 凑合 | 🟡 | P2 (用 asynq?) |
| CDN | 没 (前端静态资源直挂 nginx) | 🚧 | P2 |
| Capacity planning | docs/CAPACITY_10K_TPS.md 静态 | 🟡 | P1 (auto) |

---

## 四、灾备 (Disaster Recovery)

| 维度 | 现状 | 状态 | 优先级 |
|---|---|---|---|
| **DB backup** | 没自动备份策略 (生产应每天) | 🚧 缺 | **P0** |
| **Restore drill** | 没人验过能恢复 | 🚧 缺季度演练 | **P0** |
| **Chaos engineering** | 没故障注入 | 🚧 缺 | P1 |
| Multi-region | 单 region | 🚧 单点 | P1 (跨年) |
| RTO/RPO 定义 | 没 | 🚧 | P1 |
| Failover plan | 没 | 🚧 | P1 |

---

## 五、合规 / 安全

| 维度 | 现状 | 状态 | 优先级 |
|---|---|---|---|
| Audit log | 哈希链 + 7 年留存 (PCI 10.7) | ✅ | — |
| mTLS | 全栈 | ✅ | — |
| PAN 单跳 | card-center 隔离 | ✅ | — |
| OAuth2 | 本会话刚加 | ✅ | — |
| **PCI ASV 扫描** | 没安排 | 🚧 EXTERNAL (QSA) | P1 |
| **Pentest** | 没安排 | 🚧 EXTERNAL | P1 |
| Secrets rotation | KMS rotate 90d | ✅ | — |
| Vulnerability scan in CI | gosec / trivy | 🟡 partial | P2 |

---

## 落地计划 — Q3

**P0 (本月内交付):**
1. ✅ Loki + Promtail 集中日志栈 — [deploy/monitoring/loki/](../deploy/monitoring/loki/)
2. ✅ blackbox-exporter 合成监控 — [deploy/monitoring/blackbox/](../deploy/monitoring/blackbox/)
3. ✅ Feature flag SDK — [packages/payment-util/featureflag/](../packages/payment-util/featureflag/)
4. ✅ DB backup + restore drill — [deploy/backup/](../deploy/backup/)
5. ✅ Chaos mesh experiments — [deploy/chaos/](../deploy/chaos/)

**P1 (本季度内):**
6. Service dependency auto-graph from OTel
7. Error budget tracking + burn-rate alerts
8. Alert routing tree (sev1 → page; sev2 → slack)
9. Capacity planning自动 (基于历史 trend)
10. Per-service runbook 拆分
11. PagerDuty / OpsGenie 集成
12. DR plan + 跨年 multi-region

**P2 (本年内):**
13. RUM 前端监控
14. Connection pool tuning audit
15. Async job queue (asynq)
16. CDN for 前端
17. Blue/green deploy
18. PCI ASV / pentest 外采

---

## 已交付一览 (本次会话)

本次直接落了 P0 全部 5 项:
1. **Loki 栈** — `deploy/monitoring/loki/{loki.yml,promtail.yml,docker-compose.yml}`
2. **Synthetic** — `deploy/monitoring/blackbox/{blackbox.yml,probe-rules.yaml}`
3. **Feature flag** — `packages/payment-util/featureflag/flag.go` + 测试
4. **Backup** — `deploy/backup/{backup-cron.yaml,restore-drill.sh}`
5. **Chaos** — `deploy/chaos/{network-loss,pod-kill,db-stall}.yaml`

后续 P1/P2 按月推。

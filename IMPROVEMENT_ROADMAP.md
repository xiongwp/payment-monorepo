# 支付平台 5 维度提升路线图

写在 reconplatform + billing-system + 7 新业务包基本完工之后，
盘点**还能往前走多远**。按 5 维度切片：扩展性 / 稳定性 / 资金安全 / 可维护性 / 研发效率。

每项打分（当前现状）+ 目标态 + 关键改进项 + ROI。

---

## 1. 扩展性 (Scalability) — 当前 6/10

### 现状

| 项 | 当前 | 上限 |
|---|---|---|
| MySQL 分片 | 10 shard / 1000 shard layout 已铺 | 单 shard 是单实例（无主从） |
| Redis | 单实例 | OOM 即全停 |
| Kafka | 单 broker | RF=1 数据丢 |
| ClickHouse | 单节点 | 归档丢 |
| Services | stateless + HPA | 无服务网格 / 跨 region |
| 业务事件 | 多走 HTTP 同步 | 雪崩风险 |
| 索引扫描 | reconplatform N+1 GET 50k 上限 | 大客户慢 |

### 改进项（按 ROI）

| # | 改进 | 工作量 | ROI | 说明 |
|---|---|---|---|---|
| 1.1 | **Redis sentinel / cluster** | 3d | 🔥🔥🔥 | 单点是当前最大 SPOF |
| 1.2 | **MySQL 主从 + 读写分离 (per shard)** | 2w | 🔥🔥🔥 | 写主读从 + 故障切主 |
| 1.3 | **Kafka 3 broker + RF=3** | 1w | 🔥🔥 | webhook DLQ / event stream 持久化 |
| 1.4 | **ClickHouse cluster (ReplicatedMergeTree)** | 1w | 🔥🔥 | 冷归档可靠 |
| 1.5 | **同步 → 异步事件总线** (kafka topic) | 3w | 🔥🔥🔥 | billing/refund 现在 HTTP 同步 → 改 consumer pattern |
| 1.6 | **跨 region active-passive** | 2 月 | 🔥🔥 | DR site，RPO 5min / RTO 30min |
| 1.7 | **Hot shard 检测 + 自动 reshard** | 3w | 🔥 | 大客户打爆单 shard 时自动迁移 |
| 1.8 | **Bulkhead — 独立 http.Client per upstream** | 3d | 🔥🔥 | 防雪崩，一个慢上游不拖死所有 |
| 1.9 | **Service mesh (Istio/Linkerd)** | 1 月 | 🔥 | 跨 region 路由 + 流量复制 |
| 1.10 | **Multi-tenant DB isolation** (super merchant 独立 shard) | 2w | 🔥 | 防大商户与小商户互相影响 |

---

## 2. 稳定性 (Stability) — 当前 6/10

### 现状

- ✓ payment-core 有 circuitbreaker
- ✓ api-gateway 有限流
- ✓ HPA / PDB 已有
- ✓ readiness/liveness probe
- ✓ merchant-webhook 10 次指数退避
- ✓ audit hash chain
- ✗ 单 region
- ✗ 大部分内部 service 没限流（gateway 才有）
- ✗ 没有 chaos engineering 自动化
- ✗ SLO/SLI 没标准化定义
- ✗ 跨服务 Saga / outbox 只有 accounting-system 有
- ✗ Graceful shutdown 不严格

### 改进项

| # | 改进 | 工作量 | ROI |
|---|---|---|---|
| 2.1 | **每服务限流 middleware (payment-mw 加)** | 2d | 🔥🔥🔥 |
| 2.2 | **重试 / 退避 helper 接 mw 包** | 1d | 🔥🔥🔥 |
| 2.3 | **Bulkhead per-upstream client pool** | 2d | 🔥🔥 |
| 2.4 | **SLO YAML + Prometheus alerts** | 1w | 🔥🔥🔥 |
| 2.5 | **Error budget tracking + burn rate alert** | 1w | 🔥🔥 |
| 2.6 | **Saga 跨服务事务** (refund→billing→accounting) | 3w | 🔥🔥🔥 |
| 2.7 | **Outbox pattern** 全服务标准化 | 2w | 🔥🔥 |
| 2.8 | **Chaos toolkit 定时注故障** | 2w | 🔥🔥 |
| 2.9 | **Graceful shutdown 严格化** (drain in-flight requests) | 3d | 🔥🔥 |
| 2.10 | **Degradation modes** — 依赖挂部分功能降级 | 2w | 🔥🔥 |
| 2.11 | **跨 region active-active** | 3 月 | 🔥 (成本巨大) |

### SLO 示例（建议从这些开始）

```yaml
slos:
  - service: payment-gateway
    indicator: P99 latency < 500ms
    target: 99.9%
    window: 30d
    alert_burn_rates: [2x_1h, 5x_5m]   # 5min 内 5x 速度烧预算 → page

  - service: billing-system
    indicator: HTTP 5xx rate < 0.1%
    target: 99.95%
    window: 30d

  - service: refund-engine
    indicator: refund → completed latency < 2min
    target: 99%
    window: 7d
```

---

## 3. 资金安全 (Financial Safety) — 当前 7/10

### 现状

- ✓ accounting-system 双账本 + TCC
- ✓ 全程 BIGINT minor unit
- ✓ KMS 加密 PAN (软件层)
- ✓ 4-eyes 大额 approval (reconplatform / clearing)
- ✓ audit hash chain
- ✓ catalog 10 条对账规则
- ✓ Invariants 声明式恒等检查
- ✓ refund_excess / duplicate_charge 检测
- ✗ HSM 没接（KMS 还是软件）
- ✗ 卡数据 e2e 加密只到 gateway，后续服务未审
- ✗ Trial balance / balance sheet 日切自动检查没接
- ✗ AML (反洗钱) 检测没有
- ✗ FX 用硬编码 stub
- ✗ HSM key rotation 没自动化
- ✗ 商户提现链路是 stub

### 改进项

| # | 改进 | 工作量 | ROI |
|---|---|---|---|
| 3.1 | **HSM (CloudHSM / Thales) 替软件 KMS** | 2w + 采购 | 🔥🔥🔥 PCI 必需 |
| 3.2 | **卡数据流向全链路 audit + mask** | 1w | 🔥🔥🔥 PCI 必需 |
| 3.3 | **Daily trial balance** (借贷平衡每日跑) | 1w | 🔥🔥🔥 资金事故早发现 |
| 3.4 | **Daily fund flow report** debit+credit=0 验证 | 3d | 🔥🔥🔥 |
| 3.5 | **AML rules** (高频小额 / 黑名单国家 / 异常模式) | 3w | 🔥🔥 合规 |
| 3.6 | **FX 真接 Reuters/OANDA + 30s 锁汇** | 2w | 🔥🔥 跨境必需 |
| 3.7 | **DB PITR + 异地备份** | 1w + DBA | 🔥🔥🔥 灾难恢复 |
| 3.8 | **Per-merchant escrow** (高风险商户独立子账户) | 2w | 🔥🔥 |
| 3.9 | **Dispute hold 单独账户** (chargeback 资金隔离) | 1w | 🔥🔥 |
| 3.10 | **Settlement timing window 自动化** (T+N) | 2w | 🔥 |
| 3.11 | **Secret rotation 自动化** (cert / api key / db pwd) | 2w | 🔥🔥 |

### Trial Balance 示例

```sql
-- 每日 23:59 跑，结果落 reconplatform diff
SELECT
    DATE(occurred_at) AS day,
    SUM(CASE WHEN event_type = 'charge' THEN amount_minor ELSE 0 END) AS debits,
    SUM(CASE WHEN event_type IN ('refund', 'chargeback') THEN amount_minor ELSE 0 END) AS credits,
    SUM(CASE WHEN event_type = 'charge' THEN amount_minor
             WHEN event_type IN ('refund', 'chargeback') THEN -amount_minor
             ELSE 0 END) AS net
FROM fee_event WHERE occurred_at >= CURDATE() - 1 AND occurred_at < CURDATE()
GROUP BY day;

-- net 跟 settlement.NetPayoutMinor 总额对不上 → P0 alert
```

---

## 4. 可维护性 (Maintainability) — 当前 5/10

### 现状

- ✓ 每个服务有基本 README
- ✓ PAYMENT_SYSTEM_GAPS 评估
- ✓ biz-admin-web 统一控制台
- ✓ audit-log 全局
- ✓ Prometheus metrics（部分）
- ✓ trace_id 透传 (payment-mw)
- ✓ 5 套单元测试
- ✗ API 文档不标准化（无 OpenAPI/Swagger）
- ✗ 没有 architecture diagram (C4)
- ✗ 没有 service dependency graph 自动生成
- ✗ 没有 runbook / incident playbook
- ✗ main.go 各服务重复 (logger init / signal / probe)
- ✗ schema migration 用 raw SQL（无 golang-migrate）
- ✗ OTel SDK 只 propagate trace_id header，没接 exporter
- ✗ 配置散落 3 处 (env / yaml / config-center)
- ✗ 错误码无 enum 标准
- ✗ API versioning 策略不明

### 改进项

| # | 改进 | 工作量 | ROI |
|---|---|---|---|
| 4.1 | **OpenAPI 3.0 / swaggo 全 9 服务** | 2w | 🔥🔥🔥 商户接入门槛 |
| 4.2 | **C4 architecture diagram (PlantUML)** | 3d | 🔥🔥 |
| 4.3 | **Service dependency graph 自动生成** (扫 Go imports) | 1w | 🔥🔥 |
| 4.4 | **Runbook 模板 + 每服务 1 份** | 1w | 🔥🔥🔥 oncall 必需 |
| 4.5 | **postmortem template** + 流程化 | 2d | 🔥🔥 |
| 4.6 | **golang-migrate / atlas schema 工具** | 1w | 🔥🔥 |
| 4.7 | **bootstrap helper 抽 main.go 重复代码** | 1w | 🔥🔥 |
| 4.8 | **OTel SDK 全服务接 Jaeger/Tempo exporter** | 2w | 🔥🔥🔥 |
| 4.9 | **配置统一收敛到 config-center** | 2w | 🔥🔥 |
| 4.10 | **错误码 enum + i18n** | 1w | 🔥 |
| 4.11 | **API versioning rules + deprecation policy** | 3d | 🔥 |
| 4.12 | **Structured log fields standard** | 3d | 🔥🔥 |
| 4.13 | **ADR (Architecture Decision Records)** | 持续 | 🔥🔥 |

### Runbook 模板

```markdown
# Service: billing-system Runbook

## SLO
- P99 < 500ms / 5xx < 0.1% / 30d availability 99.95%

## Common Alerts
### Alert: billing_5xx_high
- 含义：5xx 率 > 1% 持续 5min
- 第一反应：
    1. kubectl logs -n payment -l app=billing-system --tail=200 | grep ERROR
    2. 看 fee_event 表 insert 是否积压
    3. 看 statement aggregator cron 是否卡住
- Mitigation: kubectl rollout restart deployment/billing-system
- Escalation: 30min 未恢复 → page 资金团队 leader

## Dependencies
- ↑ Upstream: order-core (HTTP), payment-channel (HTTP)
- ↓ Downstream: shared-shard MySQL, audit-log
- Effect when down: 商户当月账单延迟，refund event 写不进库（影响其它资金流）
```

---

## 5. 研发效率 (Dev Efficiency) — 当前 6/10

### 现状

- ✓ monorepo + go.work
- ✓ 一键 biz-build-and-up.sh
- ✓ e2e bash test
- ✓ CI matrix (9 包)
- ✓ biz-admin-web 看板
- ✓ DSL 模板生成 Starlark
- ✗ 无 devcontainer (新人本地环境配置 1d+)
- ✗ 无 hot reload (改 1 行要 docker build / push / restart)
- ✗ 无 mock 第三方 (Stripe 测试用真账号？)
- ✗ 无 contract test (changing API 破坏下游不通知)
- ✗ 无 fixture data generator (e2e 数据手写)
- ✗ 无 staging 环境
- ✗ gRPC proto 改了没自动 regen client/server
- ✗ 无 feature flag (灰度发布)
- ✗ 无 traffic mirroring（refactor 风险）

### 改进项

| # | 改进 | 工作量 | ROI |
|---|---|---|---|
| 5.1 | **devcontainer.json + .vscode 配置** | 2d | 🔥🔥🔥 新人 onboard < 30min |
| 5.2 | **air / reflex hot reload** 接到所有服务 | 3d | 🔥🔥🔥 改代码 < 5s 看效果 |
| 5.3 | **Stripe/Adyen Mock Server** | 1w | 🔥🔥 e2e 不依赖真账号 |
| 5.4 | **Pact / Hoverfly contract test** | 2w | 🔥🔥 API 变更预警 |
| 5.5 | **Fixture data generator** (faker + scenario DSL) | 1w | 🔥🔥 |
| 5.6 | **Staging compose stack + 自动部署** | 1w | 🔥🔥🔥 |
| 5.7 | **buf.build proto + auto gen** | 1w | 🔥🔥 |
| 5.8 | **conventional-commits + release-please** | 3d | 🔥🔥 |
| 5.9 | **OpenFeature feature flag** | 2w | 🔥🔥 |
| 5.10 | **Traffic mirroring (diffy / nginx mirror)** | 1w | 🔥 |
| 5.11 | **PR template + code review checklist** | 1d | 🔥 |
| 5.12 | **Storybook for biz-admin-web** | 1w | 🔥 |

### devcontainer.json 模板

```jsonc
{
  "name": "payment-monorepo",
  "dockerComposeFile": ["../packages/payment-admin-web/deploy/overrides/biz-stack.yml"],
  "service": "dev",
  "customizations": {
    "vscode": {
      "extensions": [
        "golang.go",
        "ms-azuretools.vscode-docker",
        "redhat.vscode-yaml",
        "graphql.vscode-graphql"
      ]
    }
  },
  "postCreateCommand": "go work sync && bash packages/payment-admin-web/deploy/biz-build-and-up.sh"
}
```

---

## 综合优先级 — 接下来 90 天该做什么

按 ROI × 紧急程度排序：

### 第 1 个月 (P0 必做)

| 任务 | 维度 | 投入 |
|---|---|---:|
| Redis sentinel + MySQL 主从 | 扩展性 | 2w |
| HSM 接入 + 卡数据全链路 audit | 资金安全 | 3w |
| Daily trial balance + fund flow report | 资金安全 | 1w |
| 每服务限流 + 重试 helper (mw 包加) | 稳定性 | 1w |
| 每服务 README + OpenAPI | 可维护性 | 2w |
| devcontainer + hot reload | 研发效率 | 1w |

### 第 2 个月 (P1)

| 任务 | 维度 | 投入 |
|---|---|---:|
| 同步 → Kafka 异步事件总线 | 扩展性 | 3w |
| Saga 跨服务事务标准化 | 稳定性 | 3w |
| AML + FX 真接 | 资金安全 | 5w |
| OTel SDK + Jaeger 全打通 | 可维护性 | 2w |
| Stripe/Adyen Mock Server | 研发效率 | 1w |

### 第 3 个月 (P2)

| 任务 | 维度 | 投入 |
|---|---|---:|
| Kafka 3 broker + ClickHouse cluster | 扩展性 | 2w |
| Chaos toolkit 持续注故障 | 稳定性 | 2w |
| HSM key rotation + secret rotation 自动化 | 资金安全 | 2w |
| C4 + runbook 全服务 | 可维护性 | 3w |
| Staging stack + traffic mirroring | 研发效率 | 2w |

### 第 4 个月 (P3 — 跨 region / DR)

- Multi-region active-passive
- DB PITR + 跨 region replication
- Service mesh (Istio)
- Multi-tenant DB isolation

---

## 不该做的（避免过度工程）

- **不要立刻上多 region active-active** — 复杂度爆炸（数据冲突 / 时钟 / 网络分区），先 active-passive
- **不要自研 service mesh** — Istio/Linkerd 够用，自研维护成本巨大
- **不要追求 100% 测试覆盖** — 70% 单测 + 关键路径 e2e 已够，过度测试拖慢迭代
- **不要 GraphQL** — REST + OpenAPI 够用，GraphQL 在支付场景没明显优势
- **不要 NoSQL 全替 MySQL** — 资金账本必须 ACID，NoSQL 只用作 cache / queue / OLAP

---

## 度量改进效果的关键指标

跟踪每月：

| 维度 | KPI |
|---|---|
| 扩展性 | 单服务 QPS 上限 / 单实例存储上限 / 横向扩容时间 |
| 稳定性 | MTTR (Mean Time To Recover) / MTBF / 5xx rate / SLO 达成率 |
| 资金安全 | unresolved P0 diff 数 / 资金事故 $ amount / PCI audit 通过率 |
| 可维护性 | onboard 新人时间 / postmortem 数量 / 工单平均处理时长 |
| 研发效率 | PR-to-prod 时间 / 单 PR 改动行数中位数 / build+test+deploy 总耗时 |

# 6 大核心系统生产就绪评估

**评估范围**: payment-core / payment-channel / api-gateway / card-center / card-payment / order-core

**评估维度**: 安全性 / 可靠性 / 可扩展性 / 可运维性 / 可观测性 / 系统性能 / 可扩容性 / 资金安全性

**评估日期**: 2026-05-18

---

## 1. 总评矩阵

按维度给每个服务打分（1=雏形, 2=可用, 3=生产可用但有 gap, 4=生产成熟, 5=优秀）。

| 维度 | payment-core | payment-channel | api-gateway | card-center | card-payment | order-core |
|---|---|---|---|---|---|---|
| 安全性 | 3 | 2.5 | 3 | **4** | **4** | 3 |
| 可靠性 | 3 | 3 | 2.5 | 3 | 3 | **3.5** |
| 可扩展性 | 3.5 | 3 | 2.5 | 3.5 | **4** | **4** |
| 可运维性 | 3.5 | 3 | 3 | 2.5 | 2.5 | 3.5 |
| 可观测性 | **4** | 2.5 | 2.5 | 2 | 3 | 3.5 |
| 系统性能 | 3 | 2.5 | 3 | 2.5 | 3.5 | 3 |
| 可扩容性 | 3.5 | 3.5 | 3 | 3 | 3 | 3 |
| 资金安全 | 2.5 | 2.5 | 2 | **4** | 3 | 3 |
| **均值** | **3.25** | **2.81** | **2.69** | **3.06** | **3.25** | **3.31** |

**一句话总评**:
- **order-core**：业务链路最成熟（状态机 + outbox + 多 worker），但缺 Redis 缓存和转化漏斗 SLO。
- **card-payment**：单笔 Authorize 路径完整（mTLS + HSM 框架 + 熔断 + 重试 + 对账），但 Capture/Refund/Void/Settlement 缺失。
- **card-center**：合规与安全最强（KMS + AAD + 一次性 token + 审计链），但 K8s 部署 manifest 和 Prometheus 指标稀疏。
- **payment-core**：架构最稳（fx + circuit breaker + idempotency），但 retry queue 还在内存版，日志 PII 脱敏未规范。
- **payment-channel**：通道接入面广（16 个 adapter），但缺 saga / outbox / circuit breaker / 对账。
- **api-gateway**：基础骨架到位（鉴权 + 限流 + drain），但下游容错（CB / retry / fallback）几乎为零，路由热更新仅纸面。

---

## 2. 横向 Gap — 普遍存在于多个服务的问题

按"出现服务数 / 严重性"排序，**优先处理**：

### 🔴 P0 — 影响资金安全 / 上线即出事

1. **gRPC mTLS 未全面铺开**
   - 已铺开: card-center ✓, card-payment ✓, split-payment ✓（刚刚 PH3-2 完成）
   - 缺失: payment-core, payment-channel, api-gateway 内部链路, order-core
   - 风险: 集群内任意 sidecar / 被攻陷 pod 可裸调下游服务窃取 PAN/订单数据
   - 修复: `payment-util/mtls` 已存在，按 split-payment 的 PH3-2 模板挨个接入即可

2. **日志 PII 脱敏未规范化**
   - 全员问题: 6/6 都有不同程度的 PII / 卡号 / token 落 access log 风险
   - payment-core 全 payload 原样打；payment-channel 含 buyer.email/phone；order-core 仅 hardcoded 名单
   - 修复: 抽 `payment-util/piiredact` 工具包提供 reflection-based 脱敏 + 各服务 LoggingInterceptor 接入

3. **资金对账缺失或单点**
   - payment-channel: AcquirerTx 有 ExternalRefNo 但无 reconciliation worker
   - card-payment: 仅有 reconcile worker 修订 pending，无 settlement 文件对账
   - payment-core: 完全无对账，依赖上游兜底
   - 修复: 参考 split-payment 的 `ReconcileWorker` 模板，每个支付侧服务都要有 daily trial balance

4. **idempotency 一致性**
   - payment-core idempotency 完整 ✓
   - card-center 一次性 token ✓, payment-channel UNIQUE 约束 ✓
   - **order-core Confirm 无幂等校验**（重复调可能多次入账）
   - **api-gateway 幂等 header 仅传递不强制**
   - 修复: order-core 加 (charge_request_id) UNIQUE + api-gateway 强制要求商户 PI/Charge 带 Idempotency-Key 头

### 🟠 P1 — 影响可靠性 / SLO

5. **Circuit Breaker / Retry / Fallback 三件套散乱**
   - payment-core 有完整 CB ✓
   - card-payment 有 CB + retry ✓
   - **payment-channel / order-core / api-gateway 三个都缺 CB**
   - 修复: `payment-util/obsbootstrap.Circuit` 已存在，挨个接入下游 client

6. **Outbox 模式不普及**
   - split-payment 有完整 outbox（event + reversal retry）✓
   - order-core 有 accounting_outbox ✓
   - payment-channel webhook forward 同步调用，失败丢消息
   - payment-core 有 outbox 表但消费侧未实现
   - 修复: `payment-util/outbox` 抽公共，各异步路径接入

7. **DLQ 不完整**
   - split-payment refund consumer 已有 DLQ ✓
   - payment-channel 失败消息无专用死信
   - order-core webhook 处理失败无 DLQ
   - 修复: kafka 端死信 topic + DB DLQ 表两套兜底

8. **Admin HTTP 端点不一致**
   - 已统一: split-payment / card-center / refund-engine（用了 `payment-util/obsbootstrap`）✓
   - **payment-core / payment-channel / api-gateway / card-payment / order-core 各自实现，特性不齐**
   - 缺最普遍: `/debug/pprof/*`（5/6 都缺）、log-level 动态调（6/6 都缺）
   - 修复: 把 obsbootstrap.AdminServer 推广到剩余 5 个服务（前面 SHARED-3 只做了 card-center + refund-engine）

9. **Prometheus 指标定义不全**
   - payment-core 14+ 指标最丰富 ✓
   - card-center / card-payment / payment-channel 仅基础 RPC 指标，缺业务 SLO 维度（per-merchant 成功率 / per-channel latency）
   - 6/6 都缺 SLO 看板 + 告警规则模板
   - 修复: 每个服务 Grafana dashboard JSON + Prometheus AlertRule（参考 split-payment 的 deploy/prometheus/alerts.yml）

### 🟡 P2 — 影响性能 / 可扩容

10. **Redis 缓存层缺失**
    - payment-channel idempotency 缓存在进程内 LRU
    - order-core 无订单详情缓存
    - card-center Detokenize 每次都 KMS Decrypt
    - 修复: 共享 Redis 接入层（payment-util/cachelib），加 read-through pattern

11. **K8s manifest 不完整**
    - 多个服务仅有 HPA + PDB，缺 Deployment / Service / ConfigMap / NetworkPolicy
    - card-center / card-payment 都没有完整的 Deployment yaml（仅 HPA/PDB 片段）
    - 修复: 标准化模板 + Helm chart 化（payment-admin-web 已有 helm chart 可复用 schema）

12. **CI/CD 流水线缺位**
    - 已有: 大部分服务有 `.github/workflows/ci.yml`（跑测试）
    - 缺失: 完整 release pipeline（build → cosign → GitOps PR）
    - 仅 split-payment 刚做了完整 release.yml（PH3-4）
    - 修复: 把 split-payment 的 release.yml 推广到剩余 5 个，统一 release tag 命名规范

13. **跨 region / 多活架构空白**
    - 全员单 region 部署
    - card-center 注释提"单 region service"
    - order-core PHP 单币种
    - 修复: 长期规划，需要业务先决定双活 / 同城双活 / 异地容灾哪种模式

14. **OTel trace 采样配置缺失**
    - 多个服务都启了 OTel exporter，但采样率硬编 0.1
    - 高流量场景采集成本不可控
    - 修复: 接入 `payment-util` adaptive sampling（之前 P2-2 做的 tail-based + per-endpoint rule 推广）

---

## 3. 各系统单点 Gap

### 3.1 payment-core
- **P0**: Retry queue 切 DB（main.go:330 已有 TODO 注释，`routing.NewDBRetryQueue` 已铺设）
- **P1**: 日志 PII 脱敏；HPA 加 p99_latency 指标（需 prometheus-adapter）
- **P2**: 路由 fallback adapter 模式化；resource request 上调

### 3.2 payment-channel
- **P0**: 加 circuit breaker 给各 adapter（防单通道故障扩散）；加备用通道降级（P1-2 做过但需 verify）
- **P1**: Webhook forward 改异步 outbox；adapter base 类抽取（之前删了，建议恢复）
- **P2**: per-adapter 成功率 / latency 指标；slow query 日志；config 热更（adapter 凭证）

### 3.3 api-gateway
- **P0**: 加 circuit breaker + retry 给下游 gRPC client；强制 Idempotency-Key for /pay
- **P1**: admin /api/v1/configs/* 实际实现（README 说"待实现"）；distributed tracing 采样配置
- **P2**: 响应 gzip / brotli；CDN 集成；多 region 路由

### 3.4 card-center
- **P0**: 完整 K8s Deployment/Service/ConfigMap manifest（目前仅 HPA/PDB）
- **P1**: Prometheus 业务指标（Tokenize/Detokenize latency histogram、KMS call latency）
- **P1**: 显式 KMS retry/backoff + 分布式锁保护 Detokenize→MarkUsed 竞态
- **P2**: Redis 缓存（payment token decrypt）、batch tokenize API
- **P3**: 跨 region 多活、shard 热备

### 3.5 card-payment
- **P0**: **Capture / Refund / Void 实现**（目前只有 Authorize + Query）
- **P0**: HSM 签名集成（替换文件 load 私钥）
- **P0**: Settlement 文件对账（card org 日切文件下载 + 解析 + 三角对账）
- **P1**: OTel trace span 接入；callback 验签 webhook 主路径
- **P1**: 完整 K8s manifest + CI/CD（仅 HPA/PDB）
- **P2**: 卡组织连接预热；context timeout budget 统一

### 3.6 order-core
- **P0**: Confirm 等关键操作幂等键（重复调可能多次入账）
- **P1**: 业务 KPI 指标（支付成功率 by channel、端到端 p99）+ 告警规则
- **P1**: Log level 动态调（config-center 已支持但未接）
- **P2**: Redis 缓存（订单详情 / 状态）；transaction 隔离级别显式配置
- **P3**: 多币种 + 跨 region 规划

---

## 4. 资金安全专题（关键风险点）

资金类系统在普通"生产 ready"之上还有额外维度。当前盘点：

| 控制项 | payment-core | payment-channel | card-payment | order-core | card-center |
|---|---|---|---|---|---|
| 幂等 (UNIQUE 约束) | ✅ | ✅ | ✅ | ⚠️ Charge 防重，Confirm 未防重 | ✅ |
| 双账核 / Ledger | ❌ | ❌ | 部分 (reconcile) | 部分 (accounting 同事务) | N/A |
| 对账 worker | ❌ | ❌ | ✅ pending only | 外部 reconplatform | N/A |
| Settlement file 对账 | ❌ | ❌ | ❌ | ❌ | N/A |
| Reversal / Refund 完整链路 | ⚠️ 仅接口 | ✅ | ❌ 未实现 | ✅ | N/A |
| 超时单自动 Reverse | ❌ | ⚠️ Pending Query 推进 | ✅ Reconcile | ✅ ExpireWorker | N/A |
| 金额精度 (minor unit) | ⚠️ 依赖 protobuf | ✅ | ✅ | ✅ Money.MinorUnits | N/A |
| 审计签名链 | ❌ | ❌ | ❌ | ✅ admin_audit | ✅ per-shard SHA256 链 |
| KMS / HSM 加密 | ✅ kms:v1: 前缀 | ⚠️ hex key 硬配 | ❌ 私钥文件 | ❌ | ✅ |
| PII redaction | ❌ | ❌ | ✅ (PAN <1ms 内存) | ⚠️ hardcoded 名单 | ✅ |
| 防重提交 (client side) | N/A | N/A | N/A | ❌ | N/A |

**资金安全总分**: 5/12 关键控制项有覆盖空白。生产上线前必须补的 3 项：
1. Settlement file 对账（card-payment）
2. 各服务双账核 / 出口审计链
3. Reversal / Refund 完整链路（card-payment 未实现，payment-core 仅接口）

---

## 5. 优先级路线图 (P0 → P3)

### Phase A (P0, 4-6 weeks) — 上线必须

1. **P0-A1**: 把 `payment-util/mtls` + `payment-util/obsbootstrap` 推广到剩余 5 个服务（split-payment 模板已就绪）
   - 影响: 安全性 + 可运维性整体上一档
2. **P0-A2**: card-payment 实现 Capture / Refund / Void + Settlement 对账
   - 影响: 卡支付服务从"演示"到"生产"
3. **P0-A3**: payment-core retry queue 切 DB
4. **P0-A4**: order-core Confirm + 关键写路径加幂等键
5. **P0-A5**: 日志 PII 脱敏抽 `payment-util/piiredact` + 各服务接入

### Phase B (P1, 6-8 weeks) — 90 天稳定运行

6. **P1-B1**: payment-channel / order-core / api-gateway 接入 circuit breaker
7. **P1-B2**: 每个支付侧服务加 reconcile worker（daily trial balance）
8. **P1-B3**: 6 个服务统一 SLO 看板 + 告警规则（参考 split-payment alerts.yml）
9. **P1-B4**: api-gateway 强制 Idempotency-Key for /pay + admin config API 实现
10. **P1-B5**: card-payment HSM 集成

### Phase C (P2, 8-12 weeks) — 性能与扩容

11. **P2-C1**: 共享 Redis 缓存接入（payment-channel idempotency / order-core 订单 / card-center decrypt）
12. **P2-C2**: 6 个服务完整 K8s Helm chart 化
13. **P2-C3**: 每个服务都接 split-payment 的 release.yml 模板（CI/CD 统一）
14. **P2-C4**: 业务 KPI 指标全面化（per-merchant / per-channel 成功率）

### Phase D (P3, 长期)

15. **P3-D1**: 多 region / 双活架构设计
16. **P3-D2**: 多币种支持
17. **P3-D3**: Chaos engineering 常态化（split-payment 已有 PH3-6 chaos 脚本模板）

---

## 6. 建议的 P0 立即可执行项

如果只挑一周内能落地、影响最大的 3 件事：

1. **把 `payment-util/obsbootstrap.AdminServer` 推广到 payment-core / payment-channel / api-gateway / card-payment / order-core**
   - 工作量: 每个服务 ~30 行代码改动
   - 收益: 5 个服务统一拿到 /healthz /readyz /metrics /pprof / log-level 动态调
   - 已有模板: `packages/split-payment/cmd/server/main.go` + `packages/refund-engine/cmd/server/main.go`

2. **抽 `payment-util/piiredact` 公共包 + 各服务 LoggingInterceptor 接入**
   - 工作量: 包 1 天 + 各服务接入半天 × 6 = 4 天
   - 收益: 合规风险消除 + 上线前不需要专门做"日志脱敏审计"

3. **order-core Confirm 路径加幂等键 (charge_request_id UNIQUE)**
   - 工作量: 半天 schema 迁移 + service 改动
   - 收益: 防止 webhook 重复投递导致的重复入账（这是最容易出资金事故的点）

如果有 2 周时间，再加一件：
4. **把 split-payment 的 release.yml + GitOps 流程推广到其它 5 个服务**
   - 工作量: 复用模板 + 6 × ~1 天
   - 收益: 真正的 CI/CD 自动化，告别"手 docker push + 改 deployment.yaml"

---

## 7. 已完成基础设施清单（可复用资产）

避免重造轮子，以下 payment-util 公共能力 + split-payment 模板可直接复用：

| 公共能力 | 路径 | 已接入服务 | 待推广服务 |
|---|---|---|---|
| Admin HTTP (healthz/readyz/metrics/pprof/log-level) | `payment-util/obsbootstrap.AdminServer` | split-payment, card-center, refund-engine | payment-core, payment-channel, api-gateway, card-payment, order-core |
| gRPC 拦截器链 (recover/log/metrics) | `payment-util/obsbootstrap.Chain` | split-payment | 其余 5 个 |
| Circuit Breaker | `payment-util/obsbootstrap.Circuit` | split-payment | 其余 5 个 |
| Runtime + DB + Outbox 指标 | `payment-util/obsbootstrap.StartRuntimeCollectors` | split-payment | 其余 5 个 |
| OTel + OTLP exporter | `payment-util` (split-payment 已用) | split-payment, card-payment | 其余 4 个 |
| mTLS scaffolding | `payment-util/mtls` | card-center, card-payment, split-payment, user-merchant-core, payment-core(部分) | api-gateway 内部, payment-channel, order-core |
| Service registry (etcd + round_robin) | `payment-util/serviceregistry` | 全员 | — |
| Config center hot reload | `payment-util/configcenter` | payment-core, payment-channel, card-center, api-gateway, order-core | card-payment |
| K8s 部署模板 (Deployment + HPA + PDB + NetworkPolicy + ServiceMonitor) | `packages/split-payment/deploy/k8s/` | 自身完整 | 其它服务可参考 |
| Release pipeline + cosign + GitOps | `packages/split-payment/.github/workflows/release.yml` | 自身完整 | 其它 5 个待复制 |
| Chaos scenarios | `packages/split-payment/test/chaos/` | 自身 3 个脚本 | 模板可复用 |
| Cert-manager + ESO + Reloader | `packages/split-payment/deploy/k8s/` | 自身完整 | 其它服务可复用 |

---

## 8. 总结：距离正式商业上线的核心结论

**整体成熟度**: 6 个服务平均 3.06 / 5，处于"灰度可用 → 生产 ready"过渡阶段。

**绝对不能上的服务**: card-payment（缺 Capture/Refund/Void 闭环 + Settlement 对账，资金会失控）。

**勉强可上的服务**: payment-core, order-core, card-center, api-gateway, payment-channel —— 但都要在 Phase A 完成后才算"安心上线"。

**最弱环节**: 资金对账 + 跨服务 PII 脱敏 + 下游容错（CB/retry/fallback）。

**优势**:
- 基础设施分层清晰（payment-util 公共包思路正确）
- split-payment 已经先行做完 Phase 3 全套改造，可作为其它服务的对标模板
- 监控 / mTLS / Secret 管理框架已就位，剩下是 6 个服务"复制粘贴"工作

**最短路径 (90 天)**: 按 Phase A (4-6w) + Phase B (6-8w) 节奏，先 P0 后 P1，可以做到 6 个核心服务全部到达"生产成熟"（4 分以上）。Phase C / D 留给上线后持续优化。

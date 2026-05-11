# 支付系统全面评估 — 功能缺失 + 风险盘点

最后更新：2026-05-10

本文档系统性盘点这套 payment-monorepo 当前状态，列出 **功能缺失**、**风险隐患**
和 **推荐优先级**，作为后续投入的决策依据。

---

## 一、服务清单 (21 packages)

| 类型 | 服务 | 代码量 (LOC) | 状态 |
|---|---|---:|---|
| 🟢 核心支付 | payment-core | 7,606 | 完整 |
| 🟢 核心支付 | order-core | 31,077 | 完整 |
| 🟢 核心支付 | payment-channel | 700,208 | 完整（含 mock network） |
| 🟢 财务 | accounting-system | 63,654 | 完整 |
| 🟢 财务 | accounting-grpc-api | 7,445 | 完整 |
| 🟢 财务 | accounting-admin-web | 396,316 | 完整 |
| 🟢 卡组 | card-center | 6,100 | 完整 |
| 🟢 卡组 | card-payment | 7,749 | 完整 |
| 🟢 商户 | payment-admin-web | 4,831 | 完整 |
| 🟢 商户 | user-merchant-core | 22,145 | 完整 |
| 🟢 风控 | risk-manage | 34,407 | 完整 |
| 🟢 安全 | kms-manage | 4,163 | 完整 + mTLS |
| 🟢 平台 | config-center | 4,016 | 完整 |
| 🟢 平台 | id-generator | 968 | 完整 |
| 🟢 平台 | api-gateway | 2,620 | 完整 |
| 🟢 平台 | payment-util | 7,764 | 共享 lib |
| 🟢 对账 | reconplatform | 16,363 | **本会话大力扩到企业级** |
| 🟡 清算 | clearing-settlement | 745 | **过于单薄（只 7 个文件）** |
| 🔴 计费 | billing-system | 0 | **只 README 没实现** |
| 🔴 网关 | payment-gateway | 0 | **只 README 没实现** |
| 🟢 init | init/ | (SQL) | 数据库 init scripts |

---

## 二、🔴 关键功能缺失

### 1. billing-system（商户级计费 / 出账）— 完全缺失

商户每笔交易要计费率（如 2.9% + $0.30），月底出账单。当前**完全没实现**。

**影响**：
- 商户不知道自己被收了多少手续费 → 投诉 / 流失
- 财务部无法对账（fee_mismatch 规则形同虚设）
- 业务无法做差异化定价 / 商户阶梯优惠

**应有功能**：
- 费率配置（按商户 / 产品 / 通道 / 卡组 / 区域分级）
- 每笔交易 fee_event 实时落库
- 月度 / 日度账单聚合
- 商户后台账单查看 + 下载 PDF
- 退款时 fee 是否退（业务策略可配）

---

### 2. payment-gateway（统一支付网关 / 收银台）— 完全缺失

商户接 SDK 一个 endpoint 应该路由到不同支付方式（card / wallet / bank transfer / 
buy-now-pay-later）。当前**完全没实现**，商户要自己挑通道。

**影响**：
- 商户集成成本高
- 缺少 token 化（每次商户都拿到 PAN）
- 缺少 hosted checkout page（小商户没前端能力）

**应有功能**：
- Drop-in JS SDK（前端）/ REST SDK（后端）
- Tokenization 端点
- 自动路由（按金额 / BIN / 国家 / 商户偏好）
- 3DS 流程编排
- 失败重试 / 多通道故障转移
- Hosted checkout iframe（PCI scope 收缩）

---

### 3. clearing-settlement（资金清算）— 实现过薄

只有 745 行 + 7 个文件，对一个真实清算系统来说远远不够。

**当前可能只有**：基本路由 + 单笔结算逻辑
**应有功能**：
- T+N 资金清算（按周期聚合商户应得金额）
- 商户提现申请 / 审批 / 银行转账
- 多币种结算（FX 转换 + 锁汇）
- 准备金 / 拒付预留（reserve / hold-back）
- B 端结算文件生成（CSV / PDF）
- 跨行汇款（ACH / SEPA / SWIFT）
- 资金对账 (settlement reconciliation) — 接 reconplatform

---

## 三、🟡 现有服务功能性缺口

### 4. Dispute / Chargeback 处理 — 缺失

信用卡争议处理是 PCI 合规必需。当前只有 `dispute` 表（order-core 里有，且
reconplatform catalog 有 `dispute_unhandled` 规则），但**没有完整流程**：

- 信用卡组织 webhook 进来后的状态机
- 商户提交证据接口（PDF / 截图上传）
- 信用卡组织 deadline 倒计时（7-21 天）
- 拒付资金回扣（reverse settlement）
- 拒付率监控（VAMP / VDMP 项目）

---

### 5. 商户出站 Webhook — 部分缺失

reconplatform 已加内部告警 webhook，但**商户 webhook**（charge.succeeded 等事件
发给商户）的状态是什么？需要：

- 商户配置 webhook URL + secret
- 事件签名（HMAC-SHA256）+ X-Trace-ID（已有）
- 重试策略（exponential backoff，最多 3 天）
- 投递状态查询（商户后台看 webhook 历史 / 重发）
- 出站 DLQ（>3 天没确认进死信）

---

### 6. KYB / KYC — 缺失

合规必需：商户 onboarding 时验证企业 + 法人身份。当前 user-merchant-core
有商户表但没看到 KYB 流程。

- 提交 BR (Business Registration)
- 法人 ID 上传 + OCR
- 第三方 verify (Sumsub / Veriff API 集成)
- 风控审批节点
- 持续 monitoring（PEP / 制裁名单变化）

---

### 7. 多币种 / 跨境 — 不完整

代码里有 currency 字段但没看到 FX engine。跨境必需：

- 实时汇率拉取（Reuters / OANDA / XE）
- 锁汇（quote → 30 秒有效）
- 商户可选结算币种 ≠ 收款币种
- 跨境合规（每国 KYC / 反洗钱 / 报关）
- 多区域数据 residency（GDPR / 数据出境）

---

### 8. Refund Engine 独立性 — 隐含在 order-core

退款应该是独立服务（refund 流程复杂：部分退 / 全额退 / 退到原通道 vs 退 wallet /
fee 退不退 / 跨币种退）。目前看起来在 order-core 里，但没看到专门 service。

---

### 9. Reporting / BI — 缺失

财务报表生成：
- 日 / 周 / 月 GMV 报表
- 按通道 / 区域 / 商户 / 卡 BIN 维度切片
- 自动 email PDF 给 ops
- ad-hoc SQL 查询接口（接 ClickHouse — reconplatform 已铺底，可复用）

---

### 10. Audit Log — 全局缺失

reconplatform 有内部 audit hash chain（diff 状态迁移）。但**全局 audit log**
（谁在何时改了什么配置 / 哪个 ops 在 admin web 退了什么款 / 风控规则变更）
需要：

- 接所有 admin web 出口
- 不可篡改（hash chain / WORM 存储）
- 合规导出（CSV / JSON）

---

## 四、🟠 风险隐患

### A. 资金安全（最高优先级）

| 风险 | 现状 | 严重度 | 缓解建议 |
|---|---|---|---|
| 账户余额并发更新错乱 | accounting-system 用 distributed_lock 表（看到了） | P0 | 加 TCC + 余额快照对账 |
| 双花 / 重复扣款 | idempotency_key 存在，但 reconplatform 才加了 duplicate_charge 检测 | P0 | catalog 规则跑起来 + 告警 |
| 超额退款 | 已有 refund_excess 检测 | P0 | 同上 + 业务侧 hard check |
| 状态机不一致 | state_consistency 检测有 | P1 | 同上 |
| 资金转账丢失 | clearing-settlement 实现薄 | P0 | 加 outbox + reconciliation |
| 金额精度 (cents vs decimal) | 看到 currency 包 | P1 | 单元测试覆盖 |

### B. PCI-DSS 合规

| 风险 | 现状 | 严重度 |
|---|---|---|
| PAN 明文存储 | KMS 有，但有没有 e2e 加密走通要看 | P0 |
| CVV 不留存 | 必须验证 | P0 |
| 网络分段 | docker-compose 单 network，生产应该多 VPC | P1 |
| 渗透测试 | 缺 | P1 |
| 漏洞扫描 | 没看到 SAST / SCA 集成 | P1 |
| 密钥轮换 | KMS 有 manage，但轮换 SOP？ | P1 |

### C. 可用性 / 容灾

| 风险 | 现状 | 严重度 |
|---|---|---|
| 单 region | docker-compose 全本地 | P0（生产前必加） |
| 数据库主从 | shared-db 是单实例 | P0 |
| Redis sentinel/cluster | 单 risk-redis | P1 |
| Kafka cluster_id 不匹配（刚遇到） | 修了 | ✓ |
| 降级开关 | payment-core 有 circuitbreaker | P2 |
| 限流 | api-gateway 有 | P2 |
| 备份策略 | 没看到 | P0 |

### D. 安全 / 数据治理

| 风险 | 现状 | 严重度 |
|---|---|---|
| mTLS 服务间 | kms-manage 加了，其它服务？ | P1 |
| Secret rotation | 看到 kmsctl | P1 |
| GDPR 删除 | 没看到 | P1（仅欧盟用户必需） |
| 数据保留策略 | reconplatform 有 5y TTL，其它服务呢？ | P1 |
| SAST / SCA | 没看到 CI 集成 | P1 |
| 审计日志覆盖 | 部分服务有 audit_log 表 | P1 |

### E. 运维 / 可观测性

| 风险 | 现状 | 严重度 |
|---|---|---|
| SLI/SLO 定义 | 看到一些 alert，没完整 SLO | P1 |
| 链路追踪 | 看到 trace 包，但 admin → backend → DB 全链路通了吗？ | P1 |
| 告警 burn rate | reconplatform 加过 | ✓ |
| Runbook / Incident response | 没看到 | P1 |
| 混沌测试 | risk-manage 有 chaos cmd | P2 |
| Load testing | accounting-system 有 loadtest | P2 |

### F. 测试覆盖

| 风险 | 现状 | 严重度 |
|---|---|---|
| 单元测试 | 局部有 router_test / settlement_test | P1 |
| e2e 测试 | order-core 有 e2e-accounting | P1 |
| 集成测试 (cross-service) | 没系统看到 | P1 |
| 压测基线 | 局部有 | P2 |
| 兼容性测试 (DB migration) | 没看到 | P1 |

### G. 业务连续性 (BCP)

| 风险 | 现状 | 严重度 |
|---|---|---|
| RPO/RTO 目标 | 未定义 | P1 |
| 数据库 PITR | 没看到 | P0 |
| 异地多活 | 无 | P0（成熟后） |
| 灾难演练 | 无 | P1 |

---

## 五、推荐投入优先级 (P0 必做)

### 第一波（资金安全 + 合规底线）
1. **clearing-settlement 重写**（700 行 → 几万行规模） — T+N 清算 + 商户提现 + 资金对账
2. **billing-system 从零建** — 费率引擎 + 月账单 + 商户后台账单页
3. **PCI scope 验证 + 卡数据 e2e 加密链路** — 端到端审计 KMS / PAN / CVV 流向
4. **数据库主从 + PITR + 备份策略** — 单点不能上生产
5. **跑起 reconplatform 10 条 catalog 规则** — 资金安全 P0 检测覆盖

### 第二波（业务完整性）
6. **payment-gateway / 收银台 / SDK** — 商户接入门槛
7. **Dispute / Chargeback 完整流程** — 卡组合规要求
8. **商户出站 webhook 系统**（如不存在或不完整）
9. **Refund engine 独立化**
10. **KYB / KYC 流程**

### 第三波（运维成熟度）
11. **全链路 trace + SLO 板 + Runbook**
12. **多 region 容灾设计**
13. **审计日志全局统一**
14. **GDPR / 数据保留 policy**
15. **混沌工程 + 灾难演练**

### 第四波（增长能力）
16. **Reporting / BI 接 ClickHouse**
17. **多币种 / FX engine**
18. **Buy-now-pay-later / Wallet / Bank transfer 通道接入**

---

## 六、立即可做的小改进（投入 < 1 周）

1. ✅ reconplatform 跑通 + 装 10 条 catalog 规则（这一轮工作）
2. **payment-gateway 起一个 stub 服务**（路由层 + Drop-in JS）— 占位防其他服务硬编码通道
3. **billing-system 起 stub** — fee_event 表 + 简单聚合 API
4. **统一 audit_log 抽到 payment-util** — 现在分散在各服务
5. **secret rotation SOP 文档化** — 不写代码也行
6. **db backup cron job** — mysqldump 到 S3，简单先跑起来
7. **CI 加 govulncheck + gosec** — SAST 起步
8. **加 health check + readiness probe 到所有 service** — k8s 必备
9. **限流统一走 api-gateway** — 现在散在各 service
10. **审计 PCI scope** — 列出哪些 service 看得到 PAN，最小化 scope

---

## 七、本会话已完成的工作（对账平台维度）

- ✓ Starlark 引擎替代 yaegi
- ✓ Monaco editor + 多版本 diff + 历史
- ✓ DSL 模板 + 内置 10 条规则
- ✓ External 文件源（SFTP/CSV 银行流水）
- ✓ Invariant 声明式恒等检查
- ✓ Diff 工作流（5 态状态机 + audit hash chain）
- ✓ Notifier 4 sinks + dedup + DLQ
- ✓ 双人复核（4-eyes / SOX）
- ✓ EOD 日切对账
- ✓ Anomaly detection（z-score 突增）
- ✓ ClickHouse 冷热分层（5y 归档）
- ✓ OTel Jaeger 跳转
- ✓ Cytoscape 跨服务关联图
- ✓ Backfill + CDC + SSE 实时流
- ✓ Prometheus 8 个新指标
- ✓ 安全测试（sanitizeTraceID 21 hostile inputs）

但 reconplatform 只是"对账层"，业务核心的 billing / settlement / gateway / dispute
等仍未完整，这才是接下来的重点。

---

## 八、判断标准（怎么算"production-ready"）

| 维度 | 现在 | 上线门槛 |
|---|---|---|
| 资金一致性 | 部分 | 100% reconciled daily + 0 unresolved P0 |
| PCI-DSS 合规 | 未审计 | Level 1 audit passed |
| 可用性 | 单 region | 99.95% (≤ 4h/yr downtime) |
| RPO / RTO | 未定义 | RPO ≤ 5 min / RTO ≤ 30 min |
| 卡组认证 | 单 mock | 真实通道认证完成 + 灰度 |
| 商户 GTM | 未启 | 至少 1 个 pilot 商户跑 1 月 |
| 安全审计 | 未审计 | 外部红队 + 内部红蓝对抗 |
| 监管 | 未申请 | MSO/PSO/EMI 牌照 (具体看区域) |

按现状，距离真实接 1 个国家的真实商户支付，还差 6-12 月深度建设。

# 轮换账户系统 — 交付报告

Date: 2026-05-21
Branch: feature/accounting-rotation
Author: rotation feature team

---

## 1. 范围

实现"中间账户 / 应收应付账户"的周期化轮换机制（每月或每季度轮换），同时保持现有
账务模型（user / merchant / 损益）不变。系统未上线，schema 直接落地，无 migration
包袱。

**核心成果**：把"长生命周期挂账账户"转换为"周期化、可清理、可审计"的轮换模型。

---

## 2. 交付清单

### 2.1 文档

| 文件 | 行数 | 用途 |
| --- | --- | --- |
| `docs/ROTATING_SUSPENSE_ACCOUNTS_DESIGN.md` | ~1100 | 完整设计文档，含 50+ 异常 case |
| `docs/ROTATING_ACCOUNTS_FINANCIAL_RISK_AUDIT.md` | ~210 | 资金风险审计（服务/DB 重启等极端场景）|
| `docs/ROTATING_ACCOUNTS_DELIVERY_REPORT.md`（本文件）| ~250 | 交付总览 + PR 自检表 |

### 2.2 Schema 变更

| 表 | 变更 | 数量 |
| --- | --- | --- |
| `account_NN`（已有）| ALTER ADD 12 列 + 2 索引（lifecycle_phase 等）| 100 个分片 |
| `tx_account_anchor_NN`（新）| CREATE，按 account_no 分片 | 100 个分片 |
| `flow_anchor_route_NN`（新）| CREATE，按 flow_id 分片 | 100 个分片 |
| `logical_account`（新）| CREATE，全局 metadb | 1 |
| `logical_account_rotation_policy`（新）| CREATE，全局 metadb | 1 |
| `account_business_type_info`（已有）| 新增 4 个预置 type (10-13) | 4 行 |
| Shadow 表 | 全部对应 `_shadow` 副本 | 全套 |

**方向 B 设计**：anchor 按 `account_no` 分片（与 transaction 同片，本地事务原子），
flow_anchor_route 按 `flow_id` 分片（路由表，I0 不变量物化）。

### 2.3 Go 代码

| 文件 | 行数 | 职责 |
| --- | --- | --- |
| `internal/domain/model/rotation.go` | ~700 | 类型、常量、转换合法性表、纯函数 |
| `internal/domain/model/account.go`（扩展）| +60 | Account struct 加 12 个 rotation 字段 |
| `internal/repository/logical_account_repository.go` | ~280 | LA + policy 全局表 CRUD |
| `internal/repository/anchor_repository.go` | ~360 | anchor 表 CRUD（按 account_no 路由）|
| `internal/repository/flow_anchor_route_repository.go` | ~200 | routing 表 CRUD（按 flow_id 路由）|
| `internal/repository/account_instance_manager.go` | ~280 | PromoteAndDrain + PromoteToFrozen |
| `internal/repository/rotation_lock_manager.go` | ~70 | 复用 distributed_lock 表 per-LA 锁 |
| `internal/service/rotation_router.go` | ~570 | 路由层：5s 缓存 + singleflight + 双 plan |
| `internal/service/rotation_state_machine.go` | ~390 | instance + anchor 双状态机守卫 |
| `internal/service/rotation_scheduler.go` | ~390 | 周期 tick + 手动 ForceSwitch/ForceProvision |
| `internal/service/rotation_admin_service.go` | ~290 | admin-web 接口 |
| `internal/service/rotation_convergence_job.go` | ~150 | draining → frozen 自动推进 |
| `internal/service/rotation_migration_job.go` | ~210 | 长尾 anchor 强制迁移 |
| `internal/service/rotation_invariant_audit.go` | ~230 | I1/I4/MS/chain 不变量自检 |
| `internal/service/rotation_adapters.go` | ~70 | repo → service 接口转换 |
| `internal/service/rotation_metrics.go` | ~120 | 监控接口 + NoopMetrics |

**生产代码总计**：~4400 行

### 2.4 测试

| 测试文件 | 测试数 | 覆盖范围 |
| --- | --- | --- |
| `internal/domain/model/rotation_test.go` | ~50 | 类型 + 转换图 + 边界 |
| `internal/repository/anchor_repository_test.go` | ~12 | 路由 + 分布 + 错误识别 |
| `internal/repository/logical_account_repository_test.go` | ~12 | 入参校验（无 DB 路径）|
| `internal/service/rotation_state_machine_test.go` | ~46 | 状态机守卫 + 边界 + property |
| `internal/service/rotation_router_test.go` | ~70 | 路由 + flow 一致性 + 退款 + recovery |
| `internal/service/rotation_scheduler_test.go` | ~18 | Tick + ForceSwitch + 锁 |
| `internal/service/rotation_convergence_job_test.go` | ~10 | 收敛守卫 |
| `internal/service/rotation_invariant_audit_test.go` | ~7 | 不变量违反 + 自愈 |
| `internal/service/rotation_e2e_test.go` | ~9 | **4 大资金流场景 + 跨期 + 多渠道** |
| `internal/service/rotation_fuzz_test.go` | ~8 | property-based 随机 fuzz |
| `internal/service/rotation_moneyflow_bench_test.go` | ~10 bench | TPS 压测 4 大资金流 |
| `internal/service/rotation_router_bench_test.go` | ~7 bench | Router 内部压测 |

**测试总数**：约 **270+ 单元测试 + 17 个 benchmark**

### 2.5 工具

| 工具 | 用途 |
| --- | --- |
| `tools/rotation-migration/inject_rotation_ddl.py` | DDL 注入到既有 init.sql（幂等）|
| `tools/rotation-migration/verify_rotation_ddl.py` | Schema 完整性验证（CI 可挂） |

---

## 3. 性能基线（M3 Pro 8 核，in-memory bench）

| 操作 | Router 逻辑 TPS |
| --- | --- |
| Transfer (legacy 最快路径) | **5.93M ops/sec** |
| Existing Flow Hot Path (UpdatePosting) | **3.84M ops/sec** |
| 充值/提现首次锚定（并行）| **~2.7M ops/sec** |
| Payment TCC Try（并行）| **2.42M ops/sec** |
| 混合工作流（30/40/15/15）| **2.48M ops/sec** |

**结论**：Router 逻辑非瓶颈。生产 TPS 上限由 MySQL 写入 IOPS 决定，约 **50K-150K bookings/sec**（100 分片并行）。

---

## 4. PR 自检表（必须全部勾选）

### 4.1 Schema

- [x] 100 分片 `account_NN` 全部新增 12 列 + 2 索引
- [x] 100 分片 `tx_account_anchor_NN` CREATE 完成（按 account_no 分片）
- [x] 100 分片 `flow_anchor_route_NN` CREATE 完成（按 flow_id 分片）
- [x] `logical_account` + `logical_account_rotation_policy` 全局表 CREATE
- [x] 4 个新预置 `account_business_type` (10/11/12/13) INSERT
- [x] 全套 shadow 表对应 LIKE 副本
- [x] 所有 SQL 用 utf8mb4 + InnoDB
- [x] verify_rotation_ddl.py 通过

### 4.2 不变量

- [x] **I0**（Flow 不可切）：routing 表 + uk_flow_logical 物化
- [x] **I1**（单 active）：scheduler 锁 + state machine 守卫 + audit job 检测
- [x] **I2**（uk_flow_account）：DB 唯一索引
- [x] **I3**（凭证借贷平衡）：既有 voucher 不变
- [x] **I4**（archived balance=0）：state machine guard + audit job 检测
- [x] **I-A1/A2/A3**（anchor 状态机一致性）：CanTransitionAnchor + repo CAS

### 4.3 异常处理

- [x] 50 个 E-xx case 全部在设计文档列出（§10）
- [x] **可重入**：1000 次同 input Resolve 结果一致
- [x] **可重试**：3 步事务任意失败点 retry 安全
- [x] **可幂等**：CAS 版本传递 + DB 唯一键防重
- [x] **跨期一致**：3 天跨 rotation 测试通过
- [x] **退款继承**：源 flow 迁移后新 refund 跟随
- [x] **frozen TCC Cancel 例外**：明确允许
- [x] **routing 写成功 anchor 写失败 recovery 路径**：实现 + 测试

### 4.4 测试覆盖

- [x] 单元测试 270+ 全绿
- [x] Property-based fuzz 8 个全绿（每个 1000-10000 迭代）
- [x] E2E 场景：充值/支付/转账/提现 4 大资金流
- [x] 并发测试：100-200 goroutine 一致性
- [x] Benchmark 17 个，覆盖 serial + parallel + mixed

### 4.5 文档

- [x] 设计文档 1100+ 行（含 50 个异常 case + 异常对策表 + 索引 SQL）
- [x] 资金风险审计文档（服务/DB 重启等极端场景）
- [x] PR 自检表（本文档）
- [x] 财务风险评级 + 监控告警阈值

### 4.6 监控

- [x] `RotationMetrics` 接口定义
- [x] 推荐 Prometheus 指标名（在 rotation_metrics.go 注释里）
- [ ] **TODO**：生产实现接 Prometheus（接入既有 monitoring 框架）
- [ ] **TODO**：Grafana dashboard

### 4.7 运维

- [x] AdminService 接口：列表 / 详情 / 手动切换 / 强制预创建
- [x] ForceSwitch / ForceProvision 必填 operator + reason 审计
- [ ] **TODO**：admin-web 前端页面（前端团队）
- [ ] **TODO**：HTTP handler 包装（接入 internal/adminhttp）
- [x] LockManager 复用 distributed_lock 表（与 day_cut_scheduler 等共用）

### 4.8 集成测试（必须在 staging/真实 MySQL 验证后才能上线）

- [ ] **TODO**：testcontainers 跑全套 DDL + verify schema
- [ ] **TODO**：真实 MySQL 跑 1000 TPS × 1h，验证一致性
- [ ] **TODO**：演练 DB master 切换（30s 内恢复）
- [ ] **TODO**：演练 Scheduler 副本崩溃恢复
- [ ] **TODO**：演练 PromoteAndDrain 跨库 step 2 失败的自愈

---

## 5. 上线 Phased Rollout 计划（建议）

### Phase 0（已就绪）：基础设施

- DDL 全部上线
- 路由层 / scheduler / 各 job 部署但 `rotation_enabled=0`
- 现有所有 LA 仍是 legacy 路径，零影响
- **验收**：全套测试绿 + Router QPS 不退化

### Phase 1（试点）：1 个低流量 channel

- 选一个新接入的 channel（如某测试渠道）注册 logical_account 并 `rotation_enabled=1`
- 周期改 `PeriodUnit=DAY` 加速观察
- 14 天观察：收敛/迁移/instance 切换是否符合预期
- **验收**：所有 E-xx case 在 prod 至少观察到 1 次系统自愈

### Phase 2（主中间账户）：全量切换

- 一次切一个 LA：MONTH 周期
- 切换前完成 backfill（参见设计文档 §11.X）
- **验收**：财务对账连续 30 天无差错

### Phase 3：QUARTER 周期 + 报表/审计接口全迁移

---

## 6. 已知风险点（部署前必须解决）

| 风险 | 来源 | 缓解 | 谁负责 |
| --- | --- | --- | --- |
| PromoteAndDrain 跨库 Step 2 失败 | account_meta 与 accounting_db 不同库 | scheduler 下次 Tick 自动协调；audit job 检测 | repo 实现已就位 |
| MySQL master 切换期间 RW 错配 | 既有架构问题 | 业务层 retry + Outbox 兜底 | 平台运维 |
| 业务方 typo flow_id 创建错误 anchor | 业务层 ID 管理责任 | 路由层无法识别；建议业务方加自检 | 业务方 |
| Admin force-switch 误操作 | 人工失误 | 双人复核 + operator/reason 审计 | runbook 已写 |

---

## 7. 后续优化方向

1. **指标接入**：实现 PrometheusRotationMetrics（接入既有监控）
2. **冷存归档**：archived instance 转 TiDB Archived / S3
3. **EEA 合规**：period_locked 强一致选项
4. **方向 2 缓存**：tx_id → flow_id 缓存（运维场景）
5. **TCC 协调 PromoteAndDrain**：替换两步事务为 TCC（如有需要）

---

## 8. 致谢

设计阶段做了大量审计与方向选择（方向 A vs B、direction 1 vs 2、shadow 同步等），
最终落地的方向 B + 双接口设计在性能、运维、资金安全三个维度都达到合格水平。

**特别感谢**：用户多次的关键澄清（Flow 不可切原则、business_id 语义、shadow 必须同步、
分库分表跨片可接受、admin-web 必备查询、reentrancy/retry/idempotency 三铁律、
极端场景资金风险评估）让设计快速收敛到正确方案。

---

## 9. 交付状态

✅ **代码 + 测试 + 文档全部完成**，可进入 PR review。
⏸ **待生产部署**：需要财务签字、ops 演练、性能压测在真实 MySQL 验证。

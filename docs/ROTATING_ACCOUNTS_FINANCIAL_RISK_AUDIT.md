# 轮换账户系统资金风险审计

Date: 2026-05-21
Status: Audit (for review)
Scope: rotating-suspense-accounts feature, direction B implementation

---

## 1. 资金安全核心不变量（必须始终成立）

| ID | 不变量 | 物化机制 | 违反后果 |
| --- | --- | --- | --- |
| **I0** | 同一 flow 的所有操作落同一 instance | `flow_anchor_route` 表 + uk_flow_logical | 资金错记账户、对账失败 |
| **I1** | 同一 logical_account 至多 1 个 active | scheduler 锁 + state machine 守卫 | 同 flow 被路由到两个 instance |
| **I2** | (flow_id, account_no) 唯一 | uk_flow_account | 重复 anchor 导致 posting 计数错乱 |
| **I3** | 凭证借贷平衡 (∑debit = ∑credit) | 既有 voucher 校验 | 试算平衡失败，资金"凭空消失/出现" |
| **I4** | archived instance balance = 0 | state machine guard | 历史 instance 仍有钱，无法对账 |
| **I-A1** | anchor.status 转换图合法 | model.CanTransitionAnchor | 状态机错乱 → 收敛 job 误判 |
| **I-A2** | flow 的 transaction 全在 anchor.account_no（或 migration 链末端） | anchor 与 transaction 同分片 + 同事务写入 | flow 资金分裂在多个 instance |

---

## 2. 极端场景分析与防护

### 2.1 服务器重启（应用层）

| 时机 | 现象 | 防护机制 | 资金风险 |
| --- | --- | --- | --- |
| Router.Resolve 中崩溃 | caller 不知是否成功 | Resolve 是**纯函数**（无写入），retry 等价 | **零风险** |
| caller 已 Resolve 但未开始写 | 无副作用 | 重新 Resolve → 同 Plan | **零风险** |
| routing 写成功 + anchor 写失败 | routing 表存在孤儿行 | Router 恢复路径：`resolveAnchorRecovery` 检测 routing 存在 anchor 缺失 → 生成 Insert anchor plan | **零风险**（caller retry 完成） |
| routing + anchor 写成功 + transaction 写失败 | anchor.posting_count=1 但无对应流水 | Router 下次返回 UpdatePosting plan with CAS version。DB 端 uk_transaction_id 防重复 | **零风险**（透过 CAS 修正） |
| 整笔 voucher 写成功 + Outbox 失败 | 凭证已落库但下游未通知 | 既有 Outbox worker 兜底投递（ADR-0002） | **零风险**（异步最终一致） |
| Scheduler 跑到一半崩溃 | 锁未释放（持有进程 die） | Lock 实现必须带 TTL（生产 distributed_lock 表带过期；本设计假设 30s TTL） | **低风险**（最坏 30s 等下次 Tick） |
| Scheduler 在 PromoteAndDrain 中崩溃 | 部分写入 | PromoteAndDrain 是**单 SQL 事务**，要么全成功要么全回滚 | **零风险**（原子性保证） |

**保护代码**：

```go
// 恢复路径已在 rotation_router.go 中实现
func (r *router) resolveExistingFlow(...) {
    anchor, err := r.anchors.GetByFlowAndAccount(...)
    if anchor == nil {
        // 走 resolveAnchorRecovery — 重新生成 Insert plan
    }
}
```

### 2.2 DB 重启 / 网络分区

| 时机 | 现象 | 防护机制 | 资金风险 |
| --- | --- | --- | --- |
| 单 SQL 中途 reset | 连接断开，事务回滚 | InnoDB ACID 保证 | **零风险** |
| 写成功但响应未达 caller | caller 重试 → uk 冲突 | 唯一索引识别重复 + caller 读已有继续 | **零风险**（已实现 isDuplicateKeyErr） |
| Master 切换中 | 短暂不可用 | 业务方 retry；Router 是无状态可重入 | **低风险**（短暂不可用） |
| Master 切换导致丢数据（最后几笔未同步） | 数据丢失 | 出 monorepo 范围（需 MySQL replication 配置半同步） | **运维责任** |

**特别注意**：
- `PromoteAndDrain` 涉及 3 张表（旧 instance / 新 instance / logical_account）的 CAS 更新
- 若这 3 张表跨分片（logical_account 在 account_meta 库，instance 在 accounting_db_N 库）→ 跨库事务
- **风险**：跨库部分提交可能导致 logical_account.current_active_account_no 不同步
- **缓解**：repo 层实现 PromoteAndDrain 必须用以下任一策略：
  1. **TCC 协调**（推荐）：复用 ADR-0002 tcc_coordinator
  2. **两步事务 + 调和**：先 instance 切换，再 LA 更新；scheduler 下次 Tick 检测不一致并补
  3. **同库**（强约束）：让 logical_account 与 instance 同库（不分库）

### 2.3 并发写入

| 场景 | 现象 | 防护机制 | 资金风险 |
| --- | --- | --- | --- |
| 两个 caller 同时 Resolve 新 flow → 都看到 routing 不存在 → 都尝试写 | 一个成功，一个 uk_flow_logical 冲突 | DB 唯一索引 + caller 接错误后重读 → 走 Update 路径 | **零风险** |
| 两个 scheduler 实例同时跑 | 都尝试 Tick | LockManager.AcquireForLogical 互斥 | **零风险** |
| 同一 anchor 两个 caller 同时 UpdatePosting | version CAS 二选一 | repo CAS on version | **零风险**（CAS 失败 caller 重新 Resolve 重试） |
| Force migration 与正常 booking 并发 | anchor.status 同时被两边改 | CAS（MarkMigratedInTx where status=active）| **零风险**（CAS 失败一方退让） |

### 2.4 时钟问题

| 场景 | 现象 | 防护 | 资金风险 |
| --- | --- | --- | --- |
| Scheduler 机器时钟偏移（提前） | 提前切换 active | 锁 + InstanceStateMachine guard（period_end 必须 ≤ now）| **低风险**（提前几分钟无大碍） |
| Scheduler 机器时钟偏移（滞后） | 推迟切换 | 下次 Tick 自动追上 | **低风险** |
| Clock 回拨（NTP 校正） | period_end 比当前晚 | Period 是 immutable 字段（出生时确定），不会重算 | **零风险** |
| DST / 闰秒 | 跨天计算偏移 | computeNextPeriod 用 IANA tz 库 | **零风险**（用 AddDate） |

### 2.5 配置错误

| 错误 | 现象 | 防护 | 资金风险 |
| --- | --- | --- | --- |
| RotationEnabled=1 但无 policy | scheduler 报错 + 跳过 | TickError 记录 | **零风险** |
| Policy.DrainP99 > DrainHardTimeout | Validate 拒绝 | model.LogicalAccountRotationPolicy.Validate | **零风险** |
| period_unit 拼错（如 "monthly"） | Validate 拒绝 | 枚举校验 | **零风险** |
| 业务方传未注册的 logical_account_key | Router 报 ErrLogicalAccountNotRegistered | 命名前缀白名单 | **零风险** |
| 业务方传 typo flow_id | 新 anchor 建立 → 但与"真正"flow 分离 | 业务方自己的幂等 ID 责任 | **业务层风险**（accounting-system 无法识别） |

### 2.6 数据污染 / 不变量违反

| 异常 | 发现机制 | 修复路径 | 资金风险 |
| --- | --- | --- | --- |
| 双 active（I1 违反） | invariant_audit_job 每小时跑 | 转 quarantined + 人工选保留方 | **高风险**（必须立即 quarantined） |
| anchor.status=migrated 但 migrated_to=nil | Router 防御性回退到原 account_no | 不会 panic | **低风险**（一次性事件 + 监控） |
| Archived 余额 ≠ 0 | state machine guard 阻止进入 archived | 必须先核销到 RotationResidualWriteOff | **零风险**（守卫前置） |
| MigrationSuspense 净额 ≠ 0 | invariant_audit_job 全局 SQL | page ops | **高风险**（迁移逻辑 bug 警报） |
| Flow 在两个 anchor 都活跃（极端） | uk_flow_account 阻止 | repo 直接 reject 第二次 insert | **零风险** |

---

## 3. 关键失败模式 + 检测时序

### 3.1 PromoteAndDrain 不一致

**最坏情况**：跨表事务部分提交。

```
事务尝试：
  UPDATE account_NN SET phase=draining WHERE no=old AND version=v_old;  -- ✓
  UPDATE account_MM SET phase=active WHERE no=new AND version=v_new;    -- ✓
  UPDATE logical_account SET current_active=new WHERE id=la AND ver=v;  -- ✗ 跨库失败
```

**结果**：新 instance 已 active，旧 instance 已 draining，但 logical_account 仍指旧。

**检测**：
- 下一次 router.Resolve 查 LA.current_active_account_no → 取旧 → 但旧 instance 已是 draining → router 报 ErrNoActiveInstance
- Caller 收到错误 → 业务方告警

**自动修复**：
- Scheduler 下次 Tick 检测出"phase=active 的 instance" ≠ "LA.current_active_account_no"
- 触发**协调流程**：将 LA.current_active_account_no 更新为实际 phase=active 的 instance

**实现要点**（repo 层必须保证）：
```go
// PromoteAndDrain 实现的强约束：
// 选项 A（最稳）：logical_account 与 instance 同库
// 选项 B（妥协）：先写 instance（两表同库本地事务），再异步发 Outbox 更新 LA
// 选项 C（最复杂）：TCC 协调
```

### 3.2 Routing + Anchor 不一致

**已通过 resolveAnchorRecovery 完全防护**。

### 3.3 Anchor + Transaction 不一致

**方向 B 关键收益**：anchor 与 transaction **同分片**，**同一本地事务**写入 → ACID 保证。

不可能出现 anchor 写成功但 transaction 写失败（同事务内不一致）。

### 3.4 强制迁移中断

**场景**：migration job 把 anchor 标记 migrated，但生成迁移凭证失败。

**防护**：
- MarkMigratedInTx 与凭证写入是**同一事务**（在同一分片，因为 anchor 与凭证都按 account_no）
- 跨分片的迁移目标 instance 上的反向凭证需要 TCC

**未实现的兜底**（Batch 8 强制迁移会实现）：
- 凭证有 `voucher_no` 唯一键 → retry 不会重复
- migration_chain_depth > 5 → quarantined 阻止无限重试

---

## 4. 监控告警清单

资金风险的运维可视化必备：

| 指标 | 阈值 | 告警等级 |
| --- | --- | --- |
| `rotation_invariant_violation_total{type=I1}` | > 0 | **P0**（立刻 page）|
| `rotation_archived_with_nonzero_balance_count` | > 0 | **P0** |
| `rotation_migration_suspense_net_amount` | ≠ 0 | **P0** |
| `rotation_anchor_status_stuck_count{logical}` | > 0 持续 1h | **P1** |
| `rotation_open_anchor_age_p99_seconds` | > 30 * 86400 | **P1** |
| `rotation_resolve_error_total{type=NoActiveInstance}` | > 10/min | **P1** |
| `rotation_scheduler_lock_failure_rate` | > 50% | **P2**（可能多副本配置错） |
| `rotation_routing_orphan_count`（routing 存在但 anchor 缺失超过 5min） | > 100 | **P2** |
| `rotation_phase_la_current_active_mismatch_total` | > 0 | **P1**（PromoteAndDrain 不一致）|

---

## 5. 灾难恢复演练清单

每季度演练以下场景：

1. **Scheduler 进程全部挂掉 24h**：恢复后能否正确推进所有滞后的轮换？
2. **MySQL 主库切换**：Resolve QPS 是否能在 30s 内恢复？
3. **Routing 表分片误删一片**：能否从 anchor 表反向重建（备用方案）？
4. **强制迁移 100 个 anchor 同时进行**：is_zero 计算 / 收敛 job 仍正确？
5. **同一 flow 在 100 个 goroutine 并发**：所有结果一致？

---

## 6. 风险评级总结

| 维度 | 评级 | 备注 |
| --- | --- | --- |
| 单点故障下数据丢失 | 🟢 低 | DB ACID + Router 幂等 |
| 跨片不一致（PromoteAndDrain） | 🟡 中 | 需 repo 层用 TCC 或同库 |
| 业务层 typo flow_id | 🟡 中 | accounting-system 无法识别，业务方责任 |
| 长时间 archived 残留余额 | 🟢 低 | 多层守卫 |
| 并发写入 race | 🟢 低 | DB 唯一索引 + CAS |
| 时钟异常 | 🟢 低 | period 字段 immutable |
| 配置错误 | 🟢 低 | Validate 多层校验 |

**总体结论**：方向 B 设计在**单一节点故障**和**并发竞争**下资金安全。最大遗留风险是 **PromoteAndDrain 跨库事务一致性**，必须由 repo 实现层选定方案 A/B/C 之一并做对应测试。

---

## 7. 建议的下一步

1. **Batch 6 完成后**：实现 PromoteAndDrain 的具体跨库一致性方案（推荐方案 A：同库）
2. **Batch 9 invariant_audit_job**：实施所有 P0/P1 监控指标
3. **演练**：每季度跑灾难恢复演练 + 报告
4. **压测**：1000 QPS 持续 1h，期间手动重启 DB / scheduler，验证最终一致性

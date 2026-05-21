# Rotating Suspense / Receivable / Payable Accounts — Design

Date: 2026-05-21
Status: Draft (for review)
Owners: accounting-system
Related: `packages/accounting-system/internal/domain/model/account.go`, ADR-0002 (Outbox + Saga)

---

## 1. Context

中间账户 (transit, type=9)、渠道应收 (type=5)、渠道应付 (type=6) 这三类账户在
当前模型里是**单实例长生命周期**——一个 `(user_id=平台, account_business_type)`
对应**一行** `account`，永远累积写入。

这带来三个痛点：

1. **沉淀难清理**。在途交易、对账尾差、历史挂账逐年累加，没人敢动旧条目，
   `AccountTransaction` 表越拉越长，月末/季末关账人工成本高。
2. **故障污染**。若某次升级或对手方异常导致挂账逻辑错乱，错误会**永久**留在
   同一账户里，与正常流水混杂，事后追责与清理代价高。
3. **关账边界模糊**。会计上希望"本期挂账，本期结清"，但现在没有自然的"期"
   边界——`cut_date` 在流水上有，但**账户本身**没有期的概念。

**目标**：在不破坏现有 `account` / `account_transaction` / `voucher` / `TCC`
语义的前提下，为这三类账户引入**周期化轮换**——每月（应收/应付）或每季
（通用中间）开新实例承接新流量，旧实例进入 draining → frozen → archived
的收敛通路，最终可审计归档。

**非目标**：

- 不改用户账户 (type=1) / 商户账户 (type=2/3) / 损益账户 (type=4) / 手续费账户
  (type=7/8) 的生命周期模型。
- 不引入 XA / 2PC（沿用 ADR-0002 的 Outbox + Saga + TCC）。
- 不替换 GORM + MySQL 选型；不改分片策略 (`user_id % 100`、`account_no` 前3字符)。
- 不修改外部对账文件格式；外部对账继续按现有 `reconcile_event` 流程跑。

---

## 2. 核心概念

| 概念 | 定义 | 在系统里如何存 |
| --- | --- | --- |
| **Logical Account** | 业务侧看到的"一个账户"，跨周期稳定。例：`渠道应付-Alipay-CNY`。 | 新表 `logical_account` |
| **Account Instance** | Logical Account 在某个具体周期内的物理实例。复用现有 `account` 表，新增字段挂周期信息。 | 现有 `account` 表 + 新字段 |
| **Lifecycle Phase** | Instance 在周期内的阶段：`active` / `draining` / `frozen` / `archived`。**不同于现有 `status`**——`status` 描述账户能否被业务使用（启停冻），`lifecycle_phase` 描述账户在轮换链中的位置。 | `account.lifecycle_phase` (新字段) |
| **Anchor** | "这笔交易在这个 Logical Account 上落在哪个 Instance" 的绑定关系。交易第一次接触某 Logical Account 时建立，所有后续分录据此路由。 | 新表 `tx_account_anchor` |
| **Migration Suspense** | 跨期强制迁移使用的过渡科目，余额恒为零（两侧对冲）。 | 新增 `AccountBusinessType` 预置项 |
| **Period** | 一个 Instance 承接新流量的时间窗口。配置在 `logical_account.rotation_policy`。 | 新表 `logical_account_rotation_policy` |

**关键不变量** (invariant)：

- **I0（最高优先级）**：**Flow 不可切**。一个资金流（money flow）在生命周期内
  的所有账户操作必须落在同一个 Account Instance 上，绝不允许中途切换到另一个
  Instance。Flow 在**第一次接触 logical_account** 时通过 Anchor 锁定 Instance
  选择，所有后续操作通过 Anchor 查找已锁定的 Instance，无论当前 active 是哪个。
  跨期、迁移、归档都不能违反此原则。退款 / 红冲是**新 Flow**，但通过显式继承
  机制（refund-of / reverse-of）"复制"源 Flow 的 Instance 选择——也不是切。
- **I1**：任意时刻，对任一 `logical_account_key`，至多一个 Instance 处于 `active`。
- **I2**：同一 `flow_id`（= flow ID）在同一 `logical_account_key`
  上的所有分录，必须落在同一 `account_no`。这是 I0 在 anchor 表上的体现。
  Anchor 永不变更，除非走 §8 的强制迁移（即使迁移，从 Flow 视角看仍是"同一套
  账户体系"——只是该 Instance 在物理上被搬到了新位置）。
- **I3**：`Voucher.total_debit == Voucher.total_credit`（既有不变量，不变）。
  跨 Instance 迁移**必须通过 Migration Suspense** 维持单凭证内借贷平衡。
- **I4**：Instance 进入 `archived` 时余额必须为 0（或仅有可审计的小额尾差并已
  显式核销到损益账户）。

**I0 的工程实现**：
- `flow_id` 字段语义上 = **flow ID**（在 TCC 场景下三阶段共享同一 ID；
  在 booking_service 现有调用中等价于 `business_no`）
- `tx_account_anchor` 表的 `(flow_id, logical_account_id) UNIQUE` 索引
  在 DB 层强制 I0：同一 flow 不可能同时存在两个 anchor 指向不同 Instance
- 路由层在 Flow 首次操作时建立 anchor，所有后续操作必须通过 anchor 查找；
  绝不允许"先看 current active，再写 anchor"这种顺序——错则会破 I0

---

## 3. 数据模型变更

### 3.1 新表 `logical_account`

承担 "稳定逻辑账户" 角色。业务方在写入时指定 `logical_account_key`，路由层
据此找到当前 `active` 的 `account_no`。

```sql
CREATE TABLE logical_account (
  id                       BIGINT UNSIGNED PRIMARY KEY,        -- Leaf 生成
  logical_account_key      VARCHAR(64)   NOT NULL,             -- 业务稳定 key，e.g. "transit:channel-payable:alipay:CNY"
  account_type             TINYINT       NOT NULL,             -- 复用 AccountType (期望值: 5/6/9)
  account_business_type    SMALLINT      NOT NULL,             -- 复用 AccountBusinessType
  currency                 CHAR(3)       NOT NULL,
  description              VARCHAR(255)  NULL,
  rotation_enabled         TINYINT       NOT NULL DEFAULT 0,   -- 0=不轮换 (兼容旧账户) 1=轮换
  current_active_account_no VARCHAR(32)  NULL,                 -- 反范式化：当期 active 的 account_no，轮换 tick 原子更新（见 §5.2.1）
  current_active_period_end DATETIME     NULL,                 -- 当期 active 的 period_end，路由层快速判断"是否在边界附近"
  status                   TINYINT       NOT NULL DEFAULT 1,   -- 1=enabled, 0=disabled
  registered_by            VARCHAR(64)   NOT NULL,             -- 注册者，必填，用于审计
  created_at               DATETIME      NOT NULL,
  updated_at               DATETIME      NOT NULL,
  version                  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  UNIQUE KEY uk_lak (logical_account_key)
);
```

**与既有 `account` 表的关系**：
- `rotation_enabled=0` 的逻辑账户对应**且仅对应一个** `account` 行——完全兼容
  现有逻辑，路由层透明转发。
- `rotation_enabled=1` 的逻辑账户对应**多行** `account`，按周期依次扮演 active
  → draining → frozen → archived。

**严格禁止 lazy create**：路由层查 `logical_account` 未命中**必须**返回
`ErrLogicalAccountNotRegistered` 并触发告警，**绝不自动创建**。所有
`logical_account_key` 必须事先经运维平台注册（含审批 + 命名空间校验），
注册接口 `RegisterLogicalAccount(key, type, business_type, currency, ...)`
强制校验命名前缀白名单（如 `channel-payable:*`、`channel-receivable:*`、
`transit:*`），未在白名单的前缀直接拒绝。此约束防止 typo 写错 key 时悄悄
创建幽灵账户（见 E-47）。

### 3.2 新表 `logical_account_rotation_policy`

```sql
CREATE TABLE logical_account_rotation_policy (
  logical_account_id      BIGINT UNSIGNED PRIMARY KEY,
  period_unit             VARCHAR(8)   NOT NULL,  -- 'MONTH' | 'QUARTER' | 'DAY' (test only)
  period_count            INT          NOT NULL DEFAULT 1,
  rotation_anchor_tz      VARCHAR(32)  NOT NULL,  -- e.g. 'Asia/Shanghai' — 决定"月初"是几点
  drain_p99_seconds       INT          NOT NULL,  -- 业务 P99 生命周期，draining 最短保留时间
  drain_hard_timeout_secs INT          NOT NULL,  -- draining 最长保留 = drain_hard_timeout_secs，超过强制迁移
  archive_grace_secs      INT          NOT NULL DEFAULT 604800, -- frozen→archived 缓冲
  config_version          BIGINT       NOT NULL DEFAULT 1,      -- 配置变更版本，路由层用它判断是否要重读
  effective_from          DATETIME     NOT NULL,                -- 政策生效时刻 (UTC)
  updated_at              DATETIME     NOT NULL
);
```

**为什么需要 `config_version`**：见 §10 异常 case E-30（配置热变更）。

### 3.3 `account` 表扩展

```sql
ALTER TABLE account
  ADD COLUMN logical_account_id   BIGINT UNSIGNED NULL,
  ADD COLUMN period_start         DATETIME        NULL,         -- inclusive (UTC)
  ADD COLUMN period_end           DATETIME        NULL,         -- exclusive (UTC)
  ADD COLUMN lifecycle_phase      TINYINT         NOT NULL DEFAULT 0,
  ADD COLUMN draining_started_at  DATETIME        NULL,
  ADD COLUMN frozen_at            DATETIME        NULL,
  ADD COLUMN archived_at          DATETIME        NULL,
  ADD COLUMN policy_version_at_birth BIGINT       NULL,         -- 出生时绑定的 policy 版本，见 E-30
  ADD KEY idx_logical_phase (logical_account_id, lifecycle_phase),
  ADD KEY idx_phase_period_end (lifecycle_phase, period_end);
```

**`lifecycle_phase` 取值**：

```
0 = legacy        (旧账户，无轮换，等价于 rotation_enabled=0 的 logical_account)
1 = active        (当期，正常接收新交易与后续分录)
2 = draining      (拒绝新交易锚定，仅接受已锚定交易的后续分录)
3 = frozen        (任何写入禁止；等待 archive_grace 过后归档；可读)
4 = archived      (只读归档，可能转冷存储)
5 = quarantined   (异常隔离，需人工介入，见 E-22)
```

**注意**：`lifecycle_phase` 与原 `status` 字段**正交**——一个账户可以同时
`status=Active, phase=draining` 表示"业务上正常但已在排空"；或者
`status=Frozen, phase=active` 表示"风控冻结的当期账户"。

### 3.4 新表 `tx_account_anchor` — 锚点注册表

**这是整个机制的中枢**。路由层每次写入都要查它。

```sql
CREATE TABLE tx_account_anchor (
  id                    BIGINT UNSIGNED PRIMARY KEY,
  flow_id               VARCHAR(64)  NOT NULL,    -- 业务侧资金流 ID（业务 ID；同一资金流的 TCC/清算/退款共享此 ID）
  logical_account_id    BIGINT UNSIGNED NOT NULL,
  account_no            VARCHAR(32)  NOT NULL,    -- 锚定到的 instance
  direction_mask        TINYINT      NOT NULL,    -- bit0=曾借记 bit1=曾贷记，便于审计
  anchored_at           DATETIME     NOT NULL,
  last_posting_at       DATETIME     NOT NULL,
  posting_count         INT          NOT NULL DEFAULT 1,
  migrated_to_account_no VARCHAR(32) NULL,        -- 若发生过强制迁移，指向新 instance
  migration_voucher_no   VARCHAR(64) NULL,        -- 迁移凭证号，可审计
  status                 TINYINT     NOT NULL DEFAULT 1,  -- 见 §3.4.1
  reuse_source           TINYINT     NOT NULL DEFAULT 0,  -- 0=primary 1=refund-of 2=reverse-of (见 §5.5)
  reuse_source_anchor_id BIGINT UNSIGNED NULL,            -- 复用了哪条 anchor，便于审计
  migration_chain_depth  TINYINT     NOT NULL DEFAULT 0,  -- 已发生过多少次强制迁移 (见 §8.6)
  created_at             DATETIME    NOT NULL,
  updated_at             DATETIME    NOT NULL,
  version                BIGINT UNSIGNED NOT NULL DEFAULT 0,
  UNIQUE KEY uk_flow_logical (flow_id, logical_account_id),
  KEY idx_logical_status_lastpost (logical_account_id, status, last_posting_at),
  KEY idx_account_status (account_no, status)
);
```

**分片策略**：按 `flow_id` 哈希分 100 片，与 `account_transaction`
对齐，避免跨片 join。

### 3.4.1 anchor.status 生命周期（关键）

```
status 取值：
  0 = trying     — 已建锚但首笔分录还是 TCC Try 中
  1 = active     — 已有终态分录但业务未声明结算完成（仍可有后续分录）
  2 = settled    — 业务声明结算完成；不再期望任何后续分录
  3 = migrated   — 已发生强制迁移，请按 migrated_to_account_no 路由
  4 = stuck      — 自动重试耗尽进入 quarantined 等待人工
```

**状态转换规则**：

| from | to | 触发 |
| --- | --- | --- |
| trying | active | 该 anchor 任一 transaction 翻为 success |
| trying | settled | 该 anchor 全部 transaction 翻为 failed/cancelled（TCC Cancel 完成） |
| active | settled | (a) 业务显式调用 `SettleAnchor(req_id, logical_key)`；或 (b) 该 anchor 的全部 TCC 分支均为 CONFIRMED/CANCELLED 终态 |
| active | migrated | §8 强制迁移完成 |
| active | stuck | 自动重试耗尽 |
| settled | (none) | settled 是终态，不再变更 |

**"open" 的统一定义**（关键，被收敛 job 和余额检查使用）：

```
open_anchor = anchor.status ∈ {0=trying, 1=active}
```

`migrated` **不算** open（已迁移至新 instance，原 instance 责任已脱）。
`settled` 不算 open。`stuck` 也不算 open，但 instance 在 stuck 数 > 0 时
**禁止**推进到 frozen，必须先 quarantined 处理。

**谁负责打 settled**：

1. **首选**：业务方在 TCC Confirm 成功后或自然清算节点显式调用
   `SettleAnchor`。这是黄金路径。
2. **兜底**：每日跑 `anchor_settle_audit_job` 扫 `active` 且 `last_posting_at`
   超过 `drain_p99 × 0.5` 的 anchor，检查其全部 transaction 是否均为终态——
   若是则自动 settle。每次自动 settle 写 audit log。
3. **极端**：超过 `drain_hard_timeout` 仍未 settle 走 §8 强制迁移而非自动
   settle——保持账目可追溯优先于自动收尾。

### 3.5 新预置 AccountBusinessType

在 `account_business_type_info` 表中预置：

| business_type | 名称 | account_type | 用途 |
| --- | --- | --- | --- |
| 10 | `MigrationSuspense` | 9 (Transit) | 跨期强制迁移的过渡科目，余额恒为 0 |
| 11 | `ResidualWriteOff` | 4 (Platform P&L) | 归档时核销小额尾差的损益账户 |
| 12 | `RotationOpsAdjust` | 4 (Platform P&L) | 人工运维调整入口（独立科目，便于审计）|

---

## 4. 状态机

```
                ┌─────────────────┐
                │   provisioned   │  ← 由 rotation scheduler 预创建
                └────────┬────────┘
                         │ activate (上一期 period_end 到达)
                         ▼
                ┌─────────────────┐  ← 接受所有新交易锚定 + 既有交易后续分录
       ┌───────▶│     active      │
       │        └────────┬────────┘
       │                 │ rotate (period_end 到达，新期 active 就位)
       │                 ▼
       │        ┌─────────────────┐  ← 拒绝新锚定；接受既有锚定的后续分录
       │        │    draining     │     收敛检测 job 每天评估
       │        └────────┬────────┘
       │                 │ converged (无 open anchor) OR hard_timeout (强制迁移完成)
       │                 ▼
       │        ┌─────────────────┐  ← 任何写入禁止；可读
       │        │     frozen      │     等待 archive_grace
       │        └────────┬────────┘
       │                 │ grace_elapsed
       │                 ▼
       │        ┌─────────────────┐  ← 转冷存储；只读
       │        │    archived     │
       │        └─────────────────┘
       │
       │        ┌─────────────────┐  ← 检测到不变量违反；需人工介入
       └────────│   quarantined   │     可从 active/draining/frozen 转入
                └─────────────────┘
```

**允许的转换**（其他一律拒绝）：

| from | to | 触发 | 守卫条件 |
| --- | --- | --- | --- |
| (none) | provisioned | scheduler 预创建 | 在 period_end − provision_lead_time 时机由 scheduler 创建（默认 lead=24h）|
| provisioned | active | scheduler tick | 同 logical 下无其他 active；当前时刻 ≥ period_start |
| active | draining | scheduler tick | 新 active 已就位且可写；logical_account.current_active_account_no 已切换 |
| draining | frozen | converge job | 全部收敛指标满足（§7.1）|
| draining | frozen | migration job | 强制迁移完成且 open_anchor_count = 0 |
| frozen | archived | archive job | now > frozen_at + archive_grace AND balance = 0 |
| * | quarantined | ops / invariant violation | 仅人工 + 双人复核 |
| quarantined | (回到原状态) | ops 解除 | 仅人工，需重跑收敛 job 决定回到 draining/frozen 中的哪个 |

**禁止的转换**：active → frozen（必须经过 draining）、archived → 任何状态、
draining → active（不允许"复活"，必须通过 quarantined）。

### 4.1 状态转换的事务边界

每次状态转换是一次本地事务：

```
BEGIN;
  -- 检查 from 状态匹配 (CAS on version)
  UPDATE account
     SET lifecycle_phase = :to_phase,
         <phase 对应时间戳> = NOW(),
         version = version + 1
   WHERE account_no = :acc AND lifecycle_phase = :from_phase AND version = :v;
  -- 若是 active→draining，新 active 必须在同一事务里 promoted
  -- 写 tx_outbox 触发下游通知（监控、报表）
  INSERT INTO tx_outbox(...);
COMMIT;
```

通过 `version` 乐观锁防并发，未影响行数 = 0 时回滚并报警。

---

## 5. 写入路由（Booking Router）

**核心契约**：业务方调用记账接口时，**不指定 `account_no`**，而是指定
`logical_account_key + flow_id`。路由层负责把它解析为具体 `account_no`。

### 5.1 接口

```go
// 新接口（推荐）
type BookingRequest struct {
    FlowID  string                // 业务幂等 key
    LogicalAccountKey string                // 例 "transit:channel-payable:alipay:CNY"
    Direction         Direction             // Debit / Credit
    Amount            int64                 // 最小货币单位 × 100
    Currency          string
    BookingType       BookingType
    OccurredAt        time.Time             // 业务发生时间（重要：见 E-25）
    VoucherNo         string                // 由调用方在凭证维度统一指定
    Metadata          map[string]string
}

func (r *Router) Book(ctx, req BookingRequest) (*AccountTransaction, error)
```

**旧接口** (`PostByAccountNo`) 保留，**禁止用于** `rotation_enabled=1` 的逻辑账户。
若误用，路由层在写入前校验失败并返回 `ErrLegacyApiOnRotatingAccount`。

### 5.2 路由算法（写路径）

```
Book(req):
  1. 加载 logical_account by logical_account_key   ← 缓存 5s
     未命中 → ErrLogicalAccountNotRegistered（绝不 lazy create，见 §3.1）
     若 rotation_enabled=0 → 直接走 legacy 路径，返回

  2. anchor lookup（按 §5.5 优先级）
       a. 自身 lookup 命中：拿到 anchor + account_no
          - 若 status='migrated' → 串联追踪到最终 account_no（见 §8.6）
          - 校验该 account 的 lifecycle_phase ∈ {active, draining}
            （SQL 显式 WHERE lifecycle_phase IN (1,2)，禁止隐式范围）
          - 若 phase=frozen 且 booking_type ∈ {TCC_CANCEL_ONLY} → 例外允许（见 §5.6 / E-16）
          - 其他状态 → ErrAnchorOnClosedInstance + 触发紧急迁移（E-08）

       b. 复用源命中（refund / reverse posting）：参见 §5.5
          - 取被复用 anchor 的 account_no
          - 用本请求的 flow_id 新建 anchor，account_no 拷自被复用 anchor，
            reuse_source / reuse_source_anchor_id 填值，便于审计

       c. 全部未命中：建立新锚点
          - 优先读 logical_account.current_active_account_no（反范式化字段）
            若与 anchor 表分片同片 → 单片查询；否则跨片读 1 次（singleflight + 5s 缓存）
          - 若 current_active_account_no 为空：ErrNoActiveInstance，立即 page（见 E-04）
          - 在 anchor 同分片本地事务里 INSERT tx_account_anchor + INSERT account_transaction
            （uk_flow_logical 防并发重复锚定 → 见 E-01）

  3. 写 account_transaction (复用现有逻辑)
       - 复用 voucher 模型保证借贷平衡
       - 余额变更走 GORM + version CAS
       - 同事务内 UPDATE tx_account_anchor (last_posting_at, posting_count, direction_mask)

  4. 写 tx_outbox 触发下游（监控、对账事件等）

  5. COMMIT
```

### 5.2.1 active instance 反范式化与一致性

`logical_account.current_active_account_no` 是反范式化字段，目的是**避免路由
热路径跨分片查询 `account` 表**（anchor 表按 `flow_id` 分片，
`account` 按 `user_id` 分片，是不同的物理分片）。

**写入此字段的唯一入口**：rotation scheduler 在执行 active 切换的同一事务内
更新——必须满足：

```
BEGIN;
  -- 1. 老 instance 转 draining (CAS on version)
  UPDATE account SET lifecycle_phase=2, draining_started_at=NOW(), version=version+1
   WHERE account_no=:old AND lifecycle_phase=1 AND version=:v_old;
  -- 2. 新 instance 转 active (CAS on version)
  UPDATE account SET lifecycle_phase=1, version=version+1
   WHERE account_no=:new AND lifecycle_phase=0 AND version=:v_new;  -- 0=provisioned
  -- 3. logical_account 反范式化更新 (CAS on version)
  UPDATE logical_account
     SET current_active_account_no=:new, current_active_period_end=:new_end, version=version+1
   WHERE id=:lid AND version=:v_la;
COMMIT;
```

三条 UPDATE 同一本地事务，任意失败回滚。**注意**：`logical_account` 表与
`account` 表分片键不同，跨片事务怎么处理？方案是 **logical_account 表用
全局表**（与 `account_business_type_info` 一样存在 account_meta 库），不分片，
路由层缓存 5s。scheduler 跨库写入用本地事务 + 补偿——若 account 表更新成功但
logical_account 未更新，下次 scheduler tick 检测出"phase=active 的 account_no
与 current_active_account_no 不一致"自动修正并 page。

### 5.3 关键的事务边界

**一次 `Book` 调用 = 一个本地 MySQL 事务**，包含：
- `account_transaction` 插入
- `account.balance / version` 更新
- `tx_account_anchor` 插入或更新
- `tx_outbox` 插入（可选）

所有这些必须落在**同一个分片**——所以 anchor 表的分片键与 transaction 表一致（按
`flow_id` 哈希）。

**为什么不能跨片**：若 anchor 在 A 分片、transaction 在 B 分片，两次 COMMIT
之间崩溃会导致"锚已建立但流水未落"——下次写入会找到陈旧锚点指向不存在的流水。

### 5.4 凭证维度跨多个 Logical Account 怎么办？

支付场景里一笔交易常常涉及多个账户：
```
借: 用户余额 -100
贷: 渠道应付-Alipay (rotating!) +100
```

`voucher_no` 是凭证维度的 ID。路由层对**每一行分录独立做锚定查询**——同一
`flow_id` 在不同 `logical_account_id` 上可以并存多条 anchor 记录
（uk_flow_logical 联合唯一）。

对于跨账户但单凭证的写入，调用方应：
1. 先 `BeginVoucher(voucher_no)`
2. 对每行分录调 `Book(req)`，复用同一 `voucher_no` 和 `flow_id`
3. `CommitVoucher` 触发借贷平衡校验

`CommitVoucher` 在底层是一个跨分片事务——参照 ADR-0002 用 **TCC**：Try
阶段写入 `tx_account_anchor` (status='trying') + `account_transaction`
(status='processing')，Confirm 阶段批量翻成 'active' / 'success'。

### 5.5 Anchor 复用规则（refund / reverse posting）

跨期场景需要新交易"沿用"旧交易的 instance。三种来源：

| 复用语义 | 来源字段 | 业务场景 |
| --- | --- | --- |
| 自身 lookup | 本请求的 `flow_id` | TCC 各阶段、同 tx 后续分录 |
| `refund-of` | `original_request_id_root`（业务方在请求中传） | 退款引用原支付 |
| `reverse-of` | `original_transaction_id`（业务方在请求中传） | 红冲引用原账 |

**统一查找优先级**（路由层按序尝试）：

```
1. 自身 lookup → (flow_id, logical_account_id)
2. reverse-of → 用 original_transaction_id 找到原 anchor
3. refund-of → 用 original_request_id_root 找到根 anchor
4. 全部失败 → 走 §5.2 步骤 2c (建立新锚点到当期 active)
```

**关键**：复用不会让两个 `flow_id` 共享同一 anchor 行。复用的含义
是"用本请求的 flow_id **新建**一行 anchor，但 `account_no` 拷自被
复用 anchor，并填 `reuse_source`/`reuse_source_anchor_id` 标识审计"。这样
`uk_flow_logical` 约束不冲突，每个业务请求都有自己的 anchor 行，查询友好。

**注意**：若被复用 anchor 的 instance 已 `archived`，路由层会走 §8 强制
迁移路径——把被复用 anchor 也一并迁移到当前 active，再把新 anchor 锚定
到迁移后的 instance。

### 5.6 TCC Cancel 在 frozen 上的例外允许

E-16 场景：Try 在 active 期，Cancel 跨过 draining 和 frozen 边界才回来。
按 §4 默认规则 frozen 拒绝写入——但若拒绝，业务侧 TCC Cancel 会无限重试。

**例外规则**：frozen 状态下允许接受**仅一种**写入：

```
allowed_in_frozen = booking_type == TCC_CANCEL
                  AND anchor.status == 'trying'
                  AND anchor.account_no == this_account_no
```

满足时直接写入，把 anchor 翻为 `settled`。这样可保证 frozen instance 余额最终
归零，不需绕道强制迁移。**不能扩大此例外的范围**——其他任何写入仍拒绝。

为支持该规则，`account` 表上的写入守卫不是简单的 phase 校验，而是带条件的：

```
ALLOW IF phase=active
ALLOW IF phase=draining AND has-existing-anchor(req_id, this_account)
ALLOW IF phase=frozen AND booking_type=TCC_CANCEL AND anchor.status=trying
DENY  otherwise
```

---

## 6. 读路径与报表聚合

### 6.1 余额查询

业务方查"渠道应付-Alipay-CNY 当前余额"时：

```
Σ balance over all account where logical_account_id = X AND lifecycle_phase IN (active, draining, frozen)
```

— 注意 `archived` 必须**排除**（已转冷存）但能在审计接口里按需查询。

实现：在 `account_service` 加 `GetLogicalAccountBalance(logical_account_key)`，
内部并行查同一 logical 的所有未归档 instance 再求和。**强烈建议**对结果做物化
聚合（5 分钟缓存），减少对热点 logical 的 fan-out 查询压力。

**一致性约定（重要）**：fan-out 求和不是原子操作，单次查询可能跨越一次轮换或
强制迁移。**logical balance 是最终一致的，不应作为财务报表的权威来源**——
财务报表必须以日切快照 `account_balance_snapshot` 为准（已有表，按 snapshot_date
锁定）。实时 logical balance 仅用于运营 dashboard / 风控阈值判断等容忍秒级偏差
的场景。

若调用方需要"瞬时一致"余额（如争议处理），提供 `GetLogicalAccountBalanceConsistent`，
内部用 REPEATABLE READ 单事务 + 所有 instance 行锁——延迟显著更高，仅限低 QPS 用途。

### 6.2 流水查询

`account_transaction` 表里**新增**冗余字段 `logical_account_id`（写入时填，便于
跨 instance 查询）。已有的按 `account_no` 查询路径保持。

```sql
ALTER TABLE account_transaction
  ADD COLUMN logical_account_id BIGINT UNSIGNED NULL,
  ADD KEY idx_logical_occurred (logical_account_id, occurred_at);
```

### 6.3 报表 / BI 对接

外部 BI 系统现在按 `account_no` 聚合的报表必须改为按 `logical_account_id`
聚合。提供视图：

```sql
CREATE VIEW v_logical_balance AS
SELECT la.logical_account_key, la.currency,
       SUM(a.balance)          AS balance,
       SUM(a.frozen_balance)   AS frozen_balance,
       SUM(a.available_balance) AS available_balance
  FROM logical_account la
  JOIN account a ON a.logical_account_id = la.id
 WHERE a.lifecycle_phase IN (1,2,3)   -- active, draining, frozen
 GROUP BY la.logical_account_key, la.currency;
```

**注意**：单实例时代的 SQL 假设 `account_no` 唯一描述账户余额，本视图打破该
假设。所有报表迁移期间，旧 SQL 应通过 `rotation_enabled=0` 过滤来确认是否
仍能用——否则必须改写。

---

## 7. 收敛检测 Job（draining → frozen）

### 7.1 触发逻辑

每个 `draining` instance 在以下指标全部满足时可推进到 `frozen`：

| 指标 | 阈值 | 来源 |
| --- | --- | --- |
| `open_anchor_count` | = 0 | `tx_account_anchor` where account_no=X AND status IN (0=trying, 1=active)  ← 见 §3.4.1 |
| `stuck_anchor_count` | = 0 | `tx_account_anchor` where account_no=X AND status=4=stuck |
| `age_since_draining` | ≥ `drain_p99_seconds` | `account.draining_started_at` |
| `balance_drift_7d` | 余额连续 7 天无变化 | 滚动统计 |
| `balance` | = 0 | `account.balance` 实际值 ← 不变量 I4 守卫 |
| `pending_async_events` | = 0 | tx_outbox 未消费且 origin_account_no=X |

**任一不满足则保持 draining**。`stuck_anchor_count > 0` 时必须先把 instance
转 quarantined，由人工处理 stuck anchor 后再决定是否推进——不允许"绕过 stuck
直接 frozen"。

### 7.2 Job 实现

`packages/accounting-system/internal/service/rotation_convergence_job.go`：

```go
func (j *ConvergenceJob) Run(ctx context.Context) {
    instances := j.repo.ListByPhase(model.LifecyclePhaseDraining)
    for _, ins := range instances {
        m := j.measure(ctx, ins)
        if m.MeetsConvergence() {
            if err := j.advance(ctx, ins, model.LifecyclePhaseFrozen); err != nil {
                metrics.RotationAdvanceFailure.Inc()
                continue
            }
            metrics.RotationDrainingConverged.Inc()
        } else if m.AgeSecs > ins.Policy.DrainHardTimeoutSecs {
            j.scheduleForceMigration(ctx, ins)   // 见 §9
        }
    }
}
```

- 频率：每 10 分钟一次（hot path 慢一点也无妨，强制迁移有独立通道）
- 并发：跨 instance 可并行；同一 instance 只允许一个 worker 持锁
  (`SELECT ... FOR UPDATE` on `account` row)
- 失败重试：写本地 `rotation_job_run` 表记录每次 run 的指标快照，失败由
  outbox 触发告警

### 7.3 可观测指标

- `rotation_open_anchor_count{logical_account, instance}` (Gauge)
- `rotation_draining_age_seconds{logical_account, instance}` (Gauge)
- `rotation_advance_total{from_phase, to_phase, result}` (Counter)
- `rotation_stuck_anchor_age_seconds{instance}` (Histogram) — 最老挂账账龄
- `rotation_balance_at_freeze{instance}` (Gauge) — frozen 时点余额，正常应为 0
- `rotation_residual_writeoff_amount{instance}` (Counter) — 归档时核销金额

---

## 8. 强制迁移 / 长尾兜底

某些 anchor 永远不收敛（业务侧 bug、对手方失联、争议拖延）。这是必须有
明确兜底路径，否则 `draining` 状态会无限延长。

### 8.1 触发

满足以下任一条件之一时，对该 anchor 发起强制迁移：

1. `draining_age > drain_hard_timeout_secs`（默认应付/应收 = P99 × 2）
2. 人工通过运维接口标记 (`force_migrate=true`)
3. 该 instance 上 stuck 的 anchor 数 ≤ 阈值 (默认 100) — 减少长尾噪音，
   超过阈值则说明系统性问题，进入 quarantined 而非强制迁移

### 8.2 迁移流程（每个 anchor 独立一笔 Voucher）

```
1. 加 anchor 行锁 (SELECT FOR UPDATE)，校验 status='active'。
2. 解析目的 instance：同 logical 当前 active。若也 draining → 顺延到下一 active。
3. 计算"残值"：anchor 在旧 instance 上的净余额 = Σ(debit - credit) for this anchor
   （需要扫该 anchor 的所有 account_transaction）。
4. 生成迁移凭证 (新 voucher_no, flow_id=migrate:{anchor_id}):
     若残值 > 0 (旧 instance 借方挂账):
       借: MigrationSuspense (旧 instance)         +残值
       贷: 旧 instance account                      -残值
       借: 新 instance account                      +残值
       贷: MigrationSuspense (新 instance)         -残值
     若残值 < 0：方向反转。
   注意：MigrationSuspense 也是分实例的，旧/新 instance 各一笔 MigrationSuspense
   分录，确保旧 instance 在本笔结束后余额减去残值=0，且 MigrationSuspense
   的两侧（旧、新）汇总后净额=0。
5. 更新 anchor:
     status='migrated',
     migrated_to_account_no=<new instance>,
     migration_voucher_no=<voucher>
6. 写 tx_outbox 通知运维。
7. COMMIT。
```

### 8.3 迁移后继续记账

后续业务方再用同样的 `flow_id` 写入时，路由层走 §5.2 步骤 2a 的
分支：发现 `status='migrated'`，自动转写到 `migrated_to_account_no`。
**对业务侧完全透明。**

### 8.4 迁移的幂等性

`migrate:{anchor_id}` 是稳定 key，重跑会被 `uk_flow_logical` 拒绝重复插入。
路由层在 Try 阶段就检查 anchor.status，若已是 `migrated` 直接幂等返回。

### 8.5 迁移失败处理

- **步骤 1-4 失败**：本地事务回滚，anchor 仍 `active`，下次 Job 重试。
- **步骤 5-7 部分失败**：MySQL 事务保证原子。
- **重试上限**：每个 anchor 最多 5 次自动重试，超限后转 quarantined 并 page ops。

### 8.6 多次迁移与 migration_chain 串联

某些极端长尾 anchor 可能跨越多次轮换被多次迁移：A → B → C → D。`anchor` 表的
`migrated_to_account_no` 只能存最近一次跳跃。路由层在 §5.2 步骤 2a 命中
`status='migrated'` 后必须**循环跟随**至最终：

```
acc := anchor.migrated_to_account_no
for next anchor where account_no=acc AND status='migrated':
    acc = next.migrated_to_account_no
    if depth > anchor.migration_chain_depth+1: 异常告警（循环或不一致）
最终 acc 指向 status='active' 的 anchor 或 instance
```

每次迁移把 `migration_chain_depth` 加 1，超过 3 跳告警，超过 5 跳直接 stuck
转 quarantined——多代迁移本身就说明业务异常。

### 8.7 跨片迁移凭证的 TCC 协调

§8.2 步骤 4 的迁移凭证涉及四笔分录（旧 instance、新 instance、
MigrationSuspense×2）。当旧/新 instance 在不同分片时（理论上不该发生，
但若 logical_account 跨分片或 MigrationSuspense 在 account_meta 库），
凭证不能用单一本地事务。

此时按 ADR-0002 拆成 **TCC**：

- **Try**：旧 instance 上写 transaction (debit MigrationSuspense, credit 旧 instance)
  + UPDATE anchor.status='trying-migrate'；新 instance 上写 transaction
  (debit 新 instance, credit MigrationSuspense)。两次 Try 在各自分片本地提交。
- **Confirm**：UPDATE anchor.status='migrated', migrated_to_account_no=新，
  migration_chain_depth+=1。
- **Cancel**：若任一 Try 失败，红冲已 Try 的分录。

通过 ADR-0002 已有的 saga_coordinator 推进。失败补偿落到 `reconcile_event`。

---

## 9. 与既有架构的兼容性

### 9.1 不动现有账户

`rotation_enabled=0` 的所有逻辑账户（含全部用户账户、商户账户、损益账户、
手续费账户）路由层透传，行为等同今日。新表 `logical_account` 在初始化时对
现有每个 `account` 建一条 1:1 记录（自动 backfill）。

### 9.2 TCC 协议

现有 TCC 在 `tcc_transaction.account_no` 字段直接绑账户。Rotation-aware
账户的 TCC 改成绑 `logical_account_id`，Try 阶段写入时由路由层解析到具体
`account_no` 并存入 `tcc_transaction.account_no`。Confirm/Cancel 直接按
`account_no` 走，不再做路由——因为 anchor 已经确定。

### 9.3 Outbox + Saga

外发事件 schema 增加 `logical_account_id` 与 `account_no_at_event` 两个字段，
下游消费者可按需切换聚合维度。

### 9.4 对账 (`reconcile_event`)

外部对账文件通常按"渠道账户"维度（= logical account）。对账匹配时按
`logical_account_id` 聚合本系统流水后再与外部条目对齐。`reconcile_event`
表新增 `logical_account_id` 字段；旧记录用 `account.logical_account_id`
backfill。

### 9.5 分片

`account_transaction`、`tx_account_anchor` 共用 `flow_id` 哈希
分片。`account` 表的分片键仍是 `user_id % 100`——平台账户 user_id 是统一
平台 ID，所以同一 logical_account 的所有 instance 落在**同一分片**——
路由层批量查 instance 不跨片。

### 9.6 ID 生成

`logical_account_id`、`anchor.id`、新 `account.id` 全部走 Leaf 号段；`account_no`
继续按现有规则生成（保持外部可见 ID 稳定）。

---

## 10. 异常 Case 与对策

本节是文档的核心——以下场景必须在实现时全部覆盖。每个 case 标注代号
（E-xx），实现 PR 必须按代号给出测试用例和监控指标。

### 10.1 并发与竞态

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-01 | 同 `flow_id` 并发首次锚定 | 高并发首笔 | `uk_flow_logical` 唯一索引冲突 | 失败方读已存在 anchor，确认 `account_no` 一致后继续 |
| E-02 | active→draining 切换瞬间，已有请求在 active 上进行 | 切换 tick 与 booking 重叠 | 写入时 `lifecycle_phase` 从 `active` 变为 `draining` | 用 `SELECT FOR UPDATE` 加行锁；写入逻辑接受 active 或 draining（既有锚定）— 两者都允许 |
| E-03 | 两笔 Book 同时给同一 anchor 写后续分录 | 多线程消费同一 tx | account.version CAS 失败 | 重试至成功；超过 3 次报警 |
| E-04 | 没有 active instance | scheduler 故障未及时建新 | 路由层 `ErrNoActiveInstance` | (1) 路由层在凌晨预检查并 self-heal 创建临时 instance；(2) 立即 page ops；(3) 业务方收到错误后**绝不能降级到旧账户**，必须重试 |
| E-05 | 配置缓存陈旧 | 5s 缓存 + 中途轮换 | anchor 命中但 instance 状态非预期 | 缓存 miss 时强制重读；anchor 校验 phase 失败时清缓存重试一次 |
| E-06 | 多实例同时 active | 切换 bug | I1 不变量违反，监控 `count(*) where phase=1 GROUP BY logical_account_id` | 立即报警；运维介入决定哪个为正；多余的转 quarantined |

### 10.2 锚点完整性

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-07 | anchor 记录写入但 transaction 写入失败 | 分片事务边界错误 | 巡检 job 扫"无对应流水的 anchor" | 不可能发生（同事务）；若发生则 anchor 标记 `stuck`，人工修复 |
| E-08 | anchor 指向已 archived instance | 长尾交易遗漏迁移 | 路由层 phase 校验失败 | 拒写并触发紧急迁移（同 §8 但不等 timeout） |
| E-09 | anchor 物理损坏（DB 坏数据） | 极端故障 | 一致性巡检对比 transaction 表 | 从 transaction 表反向重建 anchor（取首笔时间为 anchored_at） |
| E-10 | 业务方传 `flow_id` 不稳定 | 业务实现 bug | 同一业务 tx 在 anchor 表出现多条 | 监控指标 `anchor.posting_count` 分布，多条但 count=1 是嫌疑 |

### 10.3 跨期与生命周期

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-11 | 用户讨论的 "3 天支付跨期" | 在 active 锚定，draining 期间完成 | 正常路径 | 路由层透明处理，§5.2 步骤 2a |
| E-12 | 跨期超过 drain_hard_timeout | 争议持续 180 天 | 收敛 job 发现 | 走 §8 强制迁移 |
| E-13 | 退款链跨多期 | 原支付在期 A，退款在期 B 锚定**新** anchor | retrieval | 退款必须复用原支付的 `original_flow_id`（业务方约束）；anchor 表按 root 也建索引 |
| E-14 | 预授权 6 个月后 capture | 预授权时建锚，capture 时 instance 已 archived | E-08 路径 | 同 §8.3，capture 自动落到迁移后的新 instance |
| E-15 | TCC Try 跨期到 Confirm | Try 在 active，Confirm 在 draining 之后 | 正常路径 | TCC 用 `account_no`（Try 时确定），不受 anchor 影响；anchor 在 Try 时建立，Confirm 是后续分录，phase 校验放行 |
| E-16 | TCC Cancel 跨期 | Try 在 active，超时 Cancel 在 frozen 之后 | TCC stuck list | Cancel 写入会被 frozen 拒绝 → 触发针对该 anchor 的紧急迁移 → 重试 Cancel |

### 10.4 余额与不变量

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-17 | frozen 时余额非零 | open anchor 为 0 但残值非零（手工调整遗漏） | 收敛 job 在 advance 前校验余额 | block 推进；保持 draining；告警 |
| E-18 | archive 时余额非零 | 异常情况 | archive job 校验 | block 推进；进入 quarantined |
| E-19 | 双侧迁移凭证不平 | 实现 bug | voucher 提交时既有借贷平衡校验 | 立即回滚；不可能持久化不平凭证 |
| E-20 | MigrationSuspense 全局净额不为零 | 迁移逻辑 bug | 每日全局对账 job | page ops；正常应恒为 0 |
| E-21 | 残值核销超过阈值 | 系统性问题 | `rotation_residual_writeoff_amount` 报警 | 自动核销上限 = currency × 100（如 1 元/USD 1 美分），超过转人工 |
| E-22 | I1/I2/I4 不变量违反 | 任意原因 | 巡检 job 每小时跑 | 涉事 instance 进入 quarantined；page ops；提供运维 SOP |

### 10.5 时间与配置

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-23 | period_end 边界毫秒级冲突 | 切换瞬间的 booking | OccurredAt 与 period_end 精确相等 | 用半开区间 `[period_start, period_end)`；rotation_anchor_tz 决定准确边界 |
| E-24 | 跨时区业务（UTC 与 Asia/Shanghai 差 8h） | 同 logical 多区域调用 | 配置 `rotation_anchor_tz` 后唯一 | OccurredAt 必须传，路由层按 tz 折算到本地周期边界 |
| E-25 | 客户端 OccurredAt 不可信（时钟回拨） | 调用方时钟漂移 | OccurredAt 比当前服务时间晚 5 分钟以上 | 用服务端 receive_time 作 OccurredAt（路由层强制），调用方传的字段降级为参考 |
| E-26 | period_unit 跨 DST | 时区切换日的 11/13 小时 | rotation_anchor_tz 是带 DST 的时区 | period 边界按 IANA 时区库计算，不假设 24h；测试覆盖 DST 边界日 |
| E-27 | 闰秒 / NTP 跳变 | OS 层时间异常 | 监控时钟偏差 | OccurredAt 容忍 ±5 分钟漂移；超过则拒绝 |
| E-28 | rotation_policy 中途改 period_unit | 运维变更 | 旧 instance 的 policy_version_at_birth 与当前不一致 | 旧 instance 用出生时的策略走完生命周期，新策略只影响新 provisioned 的 instance |
| E-29 | drain_hard_timeout 缩短 | 配置变更 | 同 E-28 | 同 E-28；旧 instance 不受影响；如需对存量立即生效，用 §11.X policy_override（见下方）|
| E-30 | 配置热变更未生效 | 缓存延迟 | 写入时检查 `config_version` 与 cached version 一致 | 不一致则强制 reload；写入用 reload 后版本 |
| E-47 | 业务方传 typo 的 logical_account_key | 调用方 bug | 路由层 `ErrLogicalAccountNotRegistered`（§3.1 禁止 lazy create） | 业务方修复；运维不创建任何 fallback；命名前缀白名单二次保护 |
| E-48 | logical_account 表与 account 表分片不一致导致跨片更新失败 | scheduler 切换 | 切换事务部分提交 | logical_account 用全局表（account_meta 库）；scheduler 重启时巡检自愈 (§5.2.1) |
| E-49 | TCC Cancel 想在 frozen 上跑 | 跨期 Cancel | §5.6 例外规则 | 仅 `booking_type=TCC_CANCEL AND anchor.status=trying` 放行；其他写入仍拒绝 |
| E-50 | anchor migration_chain_depth 过高 | 多代轮换的长尾 | depth > 3 告警，> 5 stuck | §8.6 串联追踪；超阈值 quarantined |

### 10.6 运维 / 灾难

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-31 | rotation scheduler 自身宕机 | 单实例故障 | 心跳缺失 | scheduler 设计为多副本 + DB 抢锁；任一实例可接管 |
| E-32 | scheduler 多副本并发激活 | 抢锁失败 | `rotation_jobs.lock_owner` | 抢锁版本号 + 周期 token，多个 candidate 同时尝试时仅一个成功 |
| E-33 | 误操作把 active 转 frozen | 人为操作 | 状态转换前置守卫 (active→frozen 禁止) | 数据库 CHECK constraint + 应用层断言 |
| E-34 | DR 故障切换后状态机错乱 | 主从切换 | 跨 region 不变量巡检 | 切换后强制重跑收敛 job；archived 状态作为最终 source-of-truth |
| E-35 | 历史数据回灌 (replay) | 灾恢 / backfill | OccurredAt 远早于现在 | replay 模式下不走轮换路由（写入 archived 是禁止的），改为人工指定 instance；replay 用专用 endpoint，普通 API 不接受过期 OccurredAt |
| E-36 | 大规模数据导入（接入新渠道） | 迁库 | 大量陈旧时间戳 | 同 E-35 |
| E-37 | sharding resharding 期间轮换 | 同时跑 resharding + rotation | 路由层读 anchor 找不到 | resharding 期间冻结 rotation scheduler；resharding 完毕再恢复 |
| E-38 | logical_account 缓存击穿 | 高 QPS 缓存 miss | 监控 cache_miss_rate | singleflight；缓存空值短 TTL；hot logical 预热 |

### 10.7 业务交互

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-39 | 业务方传旧接口写轮换账户 | 兼容期 | 路由层拒绝 `ErrLegacyApiOnRotatingAccount` | 业务方修改；运维接口可单点白名单临时放行（紧急） |
| E-40 | 凭证横跨多个 logical 但部分轮换部分不轮换 | 跨账户类型 | 正常路径 | 路由层对每行分录独立锚定，混用无影响 |
| E-41 | 红冲（reverse posting）跨期 | 错账冲销，原账在期 A | 红冲必须复用原 anchor | 通过 `original_transaction_id` 找原 anchor → 写入同 instance；若 instance 已 archived → 走强制迁移路径 |
| E-42 | 业务方查"本月发生额"语义模糊 | 报表用户认知 | n/a | 文档明确：本月发生额 = 按 `occurred_at` 跨所有 instance 聚合，不是按 instance |
| E-43 | 多币种 logical | 一个 channel 多币种 | 设计上 currency 在 logical_account 上 | 不同币种 = 不同 logical_account_key，独立轮换 |

### 10.8 性能 / 可观测

| 代号 | 场景 | 触发 | 检测 | 对策 |
| --- | --- | --- | --- | --- |
| E-44 | anchor 表成为热点 | 高 QPS | 慢查询监控 | 按 flow_id 分片；命中走主键 + uk_flow_logical |
| E-45 | logical balance fan-out 太多 instance | 长尾 archived 还要算 | 路径慢 | 余额查询排除 archived；archived 走专用审计接口 |
| E-46 | Outbox 重投放大 | Kafka rebalance | 既有幂等 | 复用现有 outbox 幂等键策略 |

---

## 11. 落地路线

### Phase 0 — 准备（不开启轮换）

1. 数据表与 DDL 上线（`logical_account`, `tx_account_anchor`,
   `logical_account_rotation_policy`，`account` 表新增字段）。
2. Backfill：每个现存 `account` 建对应 `logical_account` 行，`rotation_enabled=0`。
3. 路由层上线，所有 `rotation_enabled=0` 透传，不影响线上。
4. 监控指标全部接入；告警阈值先设宽。

**验收**：QPS、错误率、延迟与上线前一致；旧 API 全部通过；新表数据齐全。

### Phase 1 — 单一 logical 试点

1. 选一个低流量的中间账户（如新接入的某个测试渠道应付款）。
2. 修改其 `rotation_enabled=1` 并设置 `period_unit=DAY` (短周期便于观察)。
3. 业务方迁移到新 API；同时部署 fallback：如果路由层报错，业务方有 5 分钟
   降级窗口写 ops 工单（不允许自动 fallback 到旧账户）。
4. 跑 14 天，观察收敛指标分布、强制迁移频率、报表是否正确。

**验收**：所有 E-xx case 在 staging 演练过；E-04、E-12 在 prod 至少各发生 1 次
且全部由系统自愈或运维 1 小时内介入完成。

### Phase 2 — 主中间账户切换

1. 一次一个 `logical_account` 切换 `rotation_enabled=1`，先 MONTH 周期。
2. **就地转换**（推荐）：直接 ALTER 现有 `account` 行——`logical_account_id`
   挂到对应 logical_account，`lifecycle_phase=2 (draining)`，
   `draining_started_at=now()`。然后用 scheduler 创建当期 active instance
   接新流量。**不开新凭证、不挪余额**——原账户作为"前置存量"draining
   实例，open_anchors=0 + balance 收敛后正常进入 frozen → archived。

   注意：之前草稿提到的"用 RotationOpsAdjust 把期初余额搬到新 instance"是
   **错误的**——会污染当期损益。已废弃。

3. 仅当业务确实希望"新期从 0 开始统计"且会计准则允许时，才走开新 instance +
   beginning_balance 路径——此时对手方**不能用 RotationOpsAdjust** (损益)，而是
   一个新增的 `RotationCarryforward` 科目（type=2 Liability 类，余额恒为 0，
   仅用于跨期搬运记录），写一笔结转分录。这是会计 carryforward 的标准做法，
   必须财务签字后启用。

### Phase 3 — 季度账户与全量

QUARTER 周期账户切换 + 报表 / 对账 / 审计接口完全迁移。

---

## 11.X Per-Instance Policy Override（紧急运维通道）

正常情况下旧 instance 走出生时的策略（E-28）。但若发现策略错配（P99 远小于
实际，旧 instance 永远不收敛），运维需要立即对单个 instance 强制收紧：

```sql
ALTER TABLE account
  ADD COLUMN effective_hard_timeout_secs INT NULL,  -- 覆盖出生 policy 的硬超时
  ADD COLUMN override_reason VARCHAR(255) NULL,
  ADD COLUMN override_by VARCHAR(64) NULL,
  ADD COLUMN override_at DATETIME NULL;
```

设值时：
1. 运维平台双人复核 + 工单 ID 必填
2. 同步写 audit log
3. 收敛 job 下一轮看到 override 立即重新评估，可能触发即时强制迁移

约束：仅可**缩短**，不允许放宽；只能对 `draining` 状态使用。

## 11.Y Quarantined Runbook 骨架

详细 runbook 在 `docs/runbooks/rotation-ops.md`，本文档列出必备步骤：

1. **判断隔离原因**：查 `rotation_quarantine_event` 表的最新一条记录
2. **冻结写入**：确认 instance 上没有任何新写入（quarantined 自然禁止，但应再核）
3. **不变量自检**：跑 Appendix B 的全部 SQL，定位违反项
4. **决策树**：
   - 余额不平 → 走人工调整凭证（用 RotationOpsAdjust 科目，留 audit）
   - migration_chain 异常 → 手工修正 anchor.migrated_to_account_no
   - 不变量 I1 违反（多个 active）→ 确定一个保留，其他降级 + 锚点迁移
5. **解除前必须重跑**收敛 job 一次，让系统判定回到 draining 还是 frozen
6. **解除操作必须双人复核**，运维平台留痕

## 12. Open Questions

1. **logical_account_key 命名规范**：建议 `"<account_type_code>:<biz>:<channel>:<currency>"`，
   实际命名空间需要与 product/finance 团队对齐。
2. **archived 数据冷存**：用 TiDB Archived 还是导 S3？影响审计接口实现。
3. **`drain_p99` 默认值**：需要从历史 transaction 表跑统计，按 (account_type,
   business_type) 维度给一份建议表。
4. **多租户/海外合规**：欧盟可能要求 period-locking 强一致，是否对 EEA 业务的
   logical_account 额外加 `period_locked=true` 后禁止任何运维调整？
5. **MigrationSuspense 在审计报表的呈现**：财务团队是否能接受这个新科目出现
   在试算平衡表里？需要会同财务评审。

---

## 13. Review Checklist

实现 PR 必须勾选：

- [ ] 数据库迁移脚本前向 + 回滚两边都跑通
- [ ] E-01 至 E-46 每个 case 都有对应的单测或集成测试
- [ ] 不变量巡检 job (`invariant_audit_job`) 实现 I1-I4 检测
- [ ] 监控指标完整接入，dashboards 截图附在 PR
- [ ] runbook 写到 `docs/runbooks/rotation-ops.md`（含 quarantined 处理、
      手动强制迁移、紧急回退）
- [ ] 与 ADR-0002 / 现有 TCC / Outbox 兼容性论证
- [ ] 财务 / 审计团队签字（试算平衡表、对账报表样例）
- [ ] 性能压测：QPS = 现有水平 × 1.2，p99 延迟回归 < 10%

---

## Appendix A: 关键 SQL 索引

```sql
-- logical_account: 按 key 查（路由热点）
UNIQUE KEY uk_lak (logical_account_key)

-- account: 按 logical + phase 找 active
KEY idx_logical_phase (logical_account_id, lifecycle_phase)
-- 收敛 job 扫 draining
KEY idx_phase_period_end (lifecycle_phase, period_end)

-- tx_account_anchor: 路由热点查询
UNIQUE KEY uk_flow_logical (flow_id, logical_account_id)
-- 收敛 job 按 instance 扫开口 anchor
KEY idx_account_status (account_no, status)
-- 长尾告警查询
KEY idx_logical_status_lastpost (logical_account_id, status, last_posting_at)
```

## Appendix B: 不变量自检 SQL

```sql
-- I1: 每个 logical 只有一个 active
SELECT logical_account_id, COUNT(*) c
  FROM account WHERE lifecycle_phase = 1
 GROUP BY logical_account_id HAVING c > 1;

-- I4: archived 账户余额非零
SELECT account_no, balance FROM account
 WHERE lifecycle_phase = 4 AND balance != 0;

-- E-20: MigrationSuspense 全局净额
SELECT SUM(balance) FROM account
 WHERE account_business_type = 10;  -- 必须为 0

-- 长尾告警：draining 中超过 hard timeout 的 anchor
SELECT a.account_no, a.draining_started_at, COUNT(*) open_anchors
  FROM account a
  JOIN tx_account_anchor t ON t.account_no = a.account_no
 WHERE a.lifecycle_phase = 2
   AND t.status = 1
   AND TIMESTAMPDIFF(SECOND, a.draining_started_at, NOW())
       > (SELECT drain_hard_timeout_secs FROM logical_account_rotation_policy
           WHERE logical_account_id = a.logical_account_id)
 GROUP BY a.account_no;
```

## Appendix C: 与现有代码的接触点

| 现有文件 | 影响 |
| --- | --- |
| `internal/domain/model/account.go` | 新增 phase 常量、Anchor 模型、LogicalAccount 模型 |
| `internal/service/booking_service.go` | 新增 Router 入口；旧入口保留 |
| `internal/service/tcc_service.go` | Try 阶段调用 Router 解析 instance |
| `internal/infrastructure/database/manager.go` | 新增 anchor 表 / logical_account 表的 repo |
| `internal/infrastructure/kafka/producer.go` | 事件增加 logical_account_id 字段 |
| `database/metadb/init/*.sql` | 新增 DDL；reconcile_event 增字段 |
| `docs/adr/0003-rotating-suspense-accounts.md` | 本文档完成评审后转 ADR |

— end —

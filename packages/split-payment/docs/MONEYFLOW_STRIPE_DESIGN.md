# Money Flow Orchestration — Stripe-Inspired Design

参考 Stripe Connect 重新设计资金链路编排，把"账户 + 资金移动"作为一等公民，
而非 Graph 里的隐式 Movement。Graph DSL 不变（运营拖图体验保留），底层
执行模型升级到 Stripe-style 资金原语。

---

## TL;DR

| 现状 | Stripe-like 新模型 |
|---|---|
| `Movement` 隐式 (edge 实例化) | `Transfer` / `ApplicationFee` / `Payout` / `Reversal` 一等对象，独立存表 |
| 节点 `account_template` 模板字符串 | `ConnectedAccount` 实体 (KYC / capabilities / payout_destination) |
| 一次 run = 一组 movement | 一次 run = 一个 `transfer_group`，关联所有 Transfer/AppFee |
| Hold 期 = 写 unsettled 后台搬 | `Payout.method = standard / instant`，holding 由 Account 余额自然延迟 |
| 退款只能改 RunPlan.status | `Reversal` 独立对象，独立审计链 |
| 无 Stripe API version 概念 | Graph `active_version` 锁定 → 已 in-flight run 用旧版本走完 |

---

## 1. Stripe Connect 的关键设计

Stripe 的资金平台模型基于 5 个核心概念，我们的目标是把它们映射到我们的栈：

### 1.1 Connected Account
每个收款方（卖家、推广员、物流方、清结算账户）都是一个独立的 `Account` 对象：
- **type**: `standard` / `express` / `custom` — KYC 严格度递增
- **country / default_currency** — 决定 payout 走哪条银行通道
- **capabilities** — `transfers`, `card_payments`, `payouts` 各自的开通状态 (`active` / `pending` / `inactive`)
- **payout_destination** — 银行账户 / 余额钱包
- **payout_schedule** — `manual` / `daily` / `weekly`

业务侧每次产生资金流，引擎要校验 `capability.transfers == active`，否则拒掉。

### 1.2 PaymentIntent → Charge → Transfer 三段
Stripe 的资金移动拆三步：
1. **Charge**: 顾客 → 平台 (拿钱)
2. **Transfer**: 平台 → connected account (分钱)
3. **Payout**: connected account → 银行 (提现)

我们的 Graph 之前是 1→3 一锅炖，新设计要显式拆开，每步独立审计 + 独立失败重试。

### 1.3 Three Charge Strategies
Stripe Connect 提供三种 charge + transfer 组合：

**A. Direct Charge** (商户直收，平台抽 fee)
- Customer → Connected Account (直接)
- ApplicationFee → Platform
- 适合：marketplace 主营业务，平台是"中间人"

**B. Destination Charge** (平台收，平台立即转给商户)
- Customer → Platform
- Transfer → Connected Account (autopilot)
- 适合：平台需要 hold 一段时间或做风控

**C. Separate Charges and Transfers** (平台收，平台按业务规则分批转)
- Customer → Platform
- Transfer × N → Connected Accounts (按 transfer_group 关联，按业务 trigger 异步发)
- 适合：多卖家分账、推广员返佣、复杂业务规则
- **我们的主用例**

### 1.4 Transfer Group
同一笔 Charge 可能产生多次 Transfer (主卖家分钱 + 推广员返佣 + 物流费 +平台手续费)。Stripe 用 `transfer_group` 字段把它们串起来，运营可：
- 按 group 查全部资金移动
- Group-level reversal (一键回滚整组)
- 审计：能不能加起来等于原 Charge

### 1.5 Reversal (Refund 的资金侧)
退款给买家时，平台要把已分出去的钱"反向收回"。Stripe 用 Reversal 显式记录：
- `Transfer.reverse()` → 创建 Reversal，状态机 `pending → completed`
- 支持部分 reversal (`amount` < `transfer.amount`)
- 幂等：同 `idempotency_key` 不会重复 reverse

---

## 2. 我们的域模型（新增）

### 2.1 `ConnectedAccount`

```go
type ConnectedAccount struct {
    ID              string            // acct_xxx
    Type            string            // "standard" / "express" / "custom"
    Country         string            // "US" / "CN" / "DE"
    DefaultCurrency string            // "USD" / "CNY"

    BusinessProfile BusinessProfile   // 商户元信息
    PayoutDestination PayoutDestination // bank / wallet 描述
    PayoutSchedule  PayoutSchedule    // manual / daily / weekly

    Capabilities    map[string]string // "transfers": "active", "payouts": "pending"
    Status          string            // "enabled" / "restricted" / "disabled" / "rejected"

    Metadata        map[string]string // 用户自定义 KV
    CreatedAt       time.Time
    UpdatedAt       time.Time
}
```

**状态机**:
```
pending → enabled  (KYC 通过)
enabled → restricted (风控触发 / 文档过期)
restricted → enabled (整改)
* → disabled (注销)
* → rejected (拒绝入驻)
```

`enabled` 是 Transfer / Payout 唯一允许的状态。`restricted` 时仍可收钱但不可出钱。

### 2.2 `Transfer`

```go
type Transfer struct {
    ID                 string    // tr_xxx
    TransferGroup      string    // tg_yyy  (同一笔 charge 衍生的所有 transfer 共享)
    SourceAccount      string    // platform 主账户 or 另一 connected
    DestinationAccount string    // acct_xxx
    AmountMinor        int64
    Currency           string

    Description        string
    SourceTransaction  string    // 关联的 charge_id / pi_id
    ApplicationFee     string    // 关联 ApplicationFee.ID (若有)

    Status             string    // "pending" / "posted" / "failed" / "reversed"
    ReversedAmount     int64     // 累计被 reverse 多少 (允许部分 reverse)

    GraphRunID         int64     // 来源 RunPlan
    IdempotencyKey     string

    CreatedAt          time.Time
    PostedAt           time.Time
}
```

**关键设计**:
- `source_transaction` 让运营反查"这笔 transfer 来自哪笔 charge"
- `reversed_amount <= amount_minor`，支持多次部分退
- 失败的 Transfer 不算"已分账"，可重试

### 2.3 `ApplicationFee`

```go
type ApplicationFee struct {
    ID             string    // fee_xxx
    Charge         string    // 关联 charge_id
    Account        string    // 抽哪个商户的 fee (空 = 平台自营)
    AmountMinor    int64
    Currency       string
    Status         string    // "pending" / "collected" / "refunded"
    RefundedAmount int64
    GraphRunID     int64
    CreatedAt      time.Time
}
```

Stripe 把平台抽成单独建模而不是普通 Transfer，因为：
- 财务侧要分别核算"营收" vs "代收代付"
- 退款时 ApplicationFee 是否退由配置决定（默认按比例退）

### 2.4 `Payout`

```go
type Payout struct {
    ID              string    // po_xxx
    Account         string    // 哪个 connected account 提现
    AmountMinor     int64
    Currency        string
    Destination     PayoutDestination  // bank_account / wallet
    Method          string    // "standard" (T+2) / "instant" (秒到, 多 1% fee)
    Status          string    // "pending" / "in_transit" / "paid" / "failed" / "canceled"
    ArrivalDate     time.Time
    FailureCode     string    // e.g. "account_closed"
    StatementDescriptor string
    GraphRunID      int64     // 来源 (若由 graph payout 节点触发) — 也可能是 manual
    CreatedAt       time.Time
}
```

### 2.5 `Reversal`

```go
type Reversal struct {
    ID             string    // tr_rev_xxx
    Transfer       string    // 关联 Transfer.ID
    AmountMinor    int64
    Currency       string
    Reason         string    // "duplicate" / "fraudulent" / "requested_by_customer"
    Status         string    // "pending" / "succeeded" / "failed"
    IdempotencyKey string    // 幂等
    GraphRunID     int64
    CreatedAt      time.Time
}
```

---

## 3. Graph DSL 演进

向后兼容：老 graph 默认 edge.kind = `transfer`。新增字段：

### 3.1 Edge `kind`

```json
{
  "from": "src",
  "to": "seller",
  "kind": "transfer",            // ← 新, 默认值
  "rule": { "type": "remainder" }
}
```

Kind 枚举：
- `transfer` (默认): 平台 → connected account, 产生 Transfer
- `application_fee`: 平台抽成, 产生 ApplicationFee
- `payout`: connected account → 银行, 产生 Payout (一般用 hold 期满后)

### 3.2 Node `account_id_template` → 解析到 ConnectedAccount

老的 `account_template: "seller_balance/{seller_id}"` 改为：
```json
{
  "id": "seller",
  "type": "connected_account",
  "account_id_template": "{seller_id}",   // 这里 ID 直接是 ConnectedAccount.ID
  "from_attr": "seller_id"
}
```

执行时按 `seller_id` 查 ConnectedAccount.GetByMetadata("seller_id", ...) 或直接 GetByID。

### 3.3 Graph `charge_strategy`

```json
{
  "charge_strategy": "separate",  // direct / destination / separate
  ...
}
```

只在 trigger 是 `charge.succeeded` 时有效，告诉 translator：
- `direct`: 不生成 Transfer (顾客直接付商户)，只生成 ApplicationFee
- `destination`: 生成单条 Transfer (整笔→唯一 destination)
- `separate`: 走现有 graph edges 逻辑 (多 Transfer + 多 Fee)

### 3.4 Graph `active_version` + immutable snapshots

```
graphs (id, key UNIQUE, name, active_version_id, status)
graph_versions (id, graph_id, version, spec_json, immutable_at)
```

改图 → 新建 graph_version 记录；激活 → 更新 graph.active_version_id。已发起的
RunPlan 持 `graph_version_id`，永远用历史 spec 走完，避免改图影响 in-flight 资金流。

---

## 4. Translator 输出升级

老的 `Translate(graph, event) → RunPlan{Movements}` 改为：

```go
type RunPlan struct {
    ...
    TransferGroup    string             // tg_xxx, 自动生成
    Transfers        []Transfer         // edge.kind=transfer 的结果
    ApplicationFees  []ApplicationFee   // edge.kind=application_fee 的结果
    Payouts          []Payout           // edge.kind=payout 的结果 (一般 hold 期满 cron 触发)
    Movements        []Movement         // 兼容老接口, 实际是 Transfers/Fees 的 union view
}
```

Workflow Engine 执行：
1. Translate → RunPlan (新 Transfer/Fee/Payout 对象)
2. 校验 capabilities (transfers active 才下 Transfer)
3. 调 accounting-system PostBatch (走旧的原子记账, 财务侧不变)
4. 把 Transfer / Fee / Payout 落自己的表 (审计 + reversal 用)
5. Publish 事件 `transfer.created`, `application_fee.created`, `payout.created`

---

## 5. Charge Strategies 详解

### 5.1 Direct Charge

```
Customer card                                     Connected Account
     │  100.00                                         ▲
     │                                                 │ 100.00 (扣 fee)
     ▼                                                 │
   Stripe (Connect) ─── ApplicationFee 5.00 ───▶ Platform
                            (平台抽 5%)
```

Graph 配:
```json
{
  "charge_strategy": "direct",
  "edges": [
    { "from": "src", "to": "platform", "kind": "application_fee",
      "rule": { "type": "percent", "value": 500 } }
  ]
}
```

Translator 不生成 Transfer (钱直接进商户)，只生成 1 个 ApplicationFee。

### 5.2 Destination Charge

```
Customer card
     │  100.00
     ▼
  Platform ──── Transfer (auto, 100% remainder) ───▶ Connected Account
     │  ApplicationFee 5.00
     ▼
  Platform 营收
```

Graph 配:
```json
{
  "charge_strategy": "destination",
  "nodes": [
    { "id": "seller", "type": "connected_account", "account_id_template": "{seller_id}" }
  ],
  "edges": [
    { "from": "src", "to": "platform", "kind": "application_fee",
      "rule": { "type": "percent", "value": 500 } },
    { "from": "src", "to": "seller", "kind": "transfer",
      "rule": { "type": "remainder" } }
  ]
}
```

### 5.3 Separate Charges and Transfers (主用例)

```
Customer card
     │  100.00
     ▼
  Platform ──┬── Transfer to seller (60%, hold 7d)
             ├── Transfer to referrer (20%)
             ├── Transfer to logistics ($5 flat)
             └── ApplicationFee 5.00 → Platform
```

Graph 配 (即现有 designer 默认产物):
```json
{
  "charge_strategy": "separate",
  "edges": [
    { "from": "src", "to": "platform",  "kind": "application_fee", "rule": { "type": "percent", "value": 500 } },
    { "from": "src", "to": "seller",    "kind": "transfer", "rule": { "type": "percent", "value": 6000 } },
    { "from": "src", "to": "referrer",  "kind": "transfer", "rule": { "type": "percent", "value": 2000 } },
    { "from": "src", "to": "logistics", "kind": "transfer", "rule": { "type": "fixed_minor", "value": 500 } }
  ],
  "hold": { "days": 7, "applies_to": ["seller"] }
}
```

---

## 6. 状态机 / 事件

### 6.1 Transfer
```
created → posted (accounting batch 成功)
posted → reversed (Reversal 成功, amount 全退)
posted → partially_reversed (Reversal amount < transfer.amount)
created → failed (capability not active / insufficient balance)
```

### 6.2 ApplicationFee
```
pending → collected
collected → refunded (退款 graph 触发自动退 fee)
collected → partially_refunded
```

### 6.3 Payout
```
pending → in_transit (银行通道已发)
in_transit → paid (银行回传成功)
in_transit → failed (账号关闭 / 不存在)
pending → canceled (用户主动取消, 仅 standard 模式)
```

### 6.4 Reversal
```
pending → succeeded (accounting 反向记账成功)
pending → failed (源 transfer 状态非 posted / amount 不够)
```

### 6.5 Events 上抛 (Webhook)

每条状态机迁移触发 Kafka topic `recon.moneyflow.events`:
```
transfer.created
transfer.posted
transfer.failed
transfer.reversed
application_fee.collected
application_fee.refunded
payout.created
payout.paid
payout.failed
reversal.succeeded
```

下游 dispatcher 按商户配置 webhook URL fan-out（复用 merchant-webhook 服务）。

---

## 7. 退款的资金侧 (Reversal flow)

Stripe 退款两段：

1. **Refund** (买家侧): 已建模 (refund-engine)
2. **Reversal** (商户侧): 新建模

退款事件 `refund.completed` 触发：
1. 查 `charge.transfer_group`，列出原 group 下所有 Transfer
2. 按 `reversal.strategy = proportional` 计算每条 Transfer 应 reverse 多少
3. 对每条 Transfer 创建 Reversal (幂等 key = refund_id + transfer_id)
4. ApplicationFee 也按比例 refund
5. 全部 succeeded → 退款资金流闭环

Graph 配:
```json
{
  "reversal": {
    "strategy": "proportional",          // proportional / fixed_from_platform
    "platform_covers_shortfall": true,    // 卖家余额不足时平台垫付
    "refund_application_fee": true        // 同步 refund 抽成
  }
}
```

---

## 8. 与现有架构的映射

| Stripe 概念 | 我们的实现 |
|---|---|
| Charge | `order-core.payment_intent` + `payment-channel.acquirer_tx` |
| Transfer | 新表 `transfers` + accounting-system 原子记账批次 |
| ApplicationFee | 新表 `application_fees` |
| Payout | `clearing-settlement.payout_request` (已存在) + 新表 `payouts` 关联 |
| ConnectedAccount | `user-merchant-core.merchant` (扩展 capabilities 字段) |
| Reversal | 新表 `reversals` + 反向 accounting batch |
| TransferGroup | 新字段, 自动生成 |
| Webhook | `merchant-webhook` 服务 (已存在, 加新 event type) |

---

## 9. Phase 1 实施清单 (本 PR 范围)

- [x] 设计文档 (本文件)
- [ ] `domain/account.go` — ConnectedAccount
- [ ] `domain/transfer.go` — Transfer / ApplicationFee / Payout / Reversal
- [ ] Graph DSL 扩展: `Edge.Kind`, `Graph.ChargeStrategy`, `Graph.ActiveVersion`
- [ ] MySQL schema: `connected_accounts` / `transfers` / `application_fees` / `payouts` / `reversals`
- [ ] Repo 接口 + MySQL 实现
- [ ] Translator 输出升级: 输出 typed `Transfers/Fees/Payouts`
- [ ] HTTP API `/api/connected_accounts/*`, `/api/transfers/*`, etc.
- [ ] Designer UI: edge `kind` 下拉, "Accounts" 管理 tab

## 10. Phase 2 (下个迭代)

- [ ] Versioned graphs (active_version + 不可变 snapshot)
- [ ] Capability gates (校验 account capability 才允许 Transfer)
- [ ] Multi-currency + FX rate snapshot
- [ ] Webhook event publish + 重试 + DLQ
- [ ] 4-eyes approval for graph activation
- [ ] Reversal 自动 trigger from refund-engine
- [ ] Payout 调度 (cron + 银行通道路由)
- [ ] Hold-period unstick worker (T+N 到期搬钱)

## 11. Phase 3

- [ ] AML / Sanctions 检查 hook
- [ ] Disputes 关联 (charge → transfer → reversal 链路)
- [ ] Programmatic API (SDK / OpenAPI)
- [ ] Real Stripe API 兼容层 (acct_xxx / tr_xxx / fee_xxx ID 格式)

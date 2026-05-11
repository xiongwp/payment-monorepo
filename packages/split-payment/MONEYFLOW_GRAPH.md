# Money Flow Graph

不是简单的分账, 是**通用资金流编排** — 用户在 payment-admin 拖拽设计资金流, workflow engine 监听业务事件自动触发, 翻译成 accounting-system 的原子批量记账。

## 架构

```
   ┌─── payment-admin Graph Designer ────┐
   │  cytoscape.js 拖拽 + 表单            │
   │  节点: 账户 (platform / merchant /   │
   │         seller / payout)             │
   │  边:   规则 (% / fixed / remainder)  │
   │  ⇒ MoneyFlowGraph JSON 存 config-    │
   │     center / split-payment DB       │
   └──────────┬──────────────────────────┘
              │
   ┌──────────▼──────────────────┐
   │  Workflow Engine             │
   │  事件 → 找匹配 graph → 执行  │
   └──────────┬──────────────────┘
              │
   ┌──────────▼─────────────────────┐
   │  Translator                     │
   │  Graph → AtomicBatchBooking     │
   └──────────┬─────────────────────┘
              │
   ┌──────────▼──────────────────────┐
   │  accounting-system               │
   │  全部原子, 失败回滚              │
   └─────────────────────────────────┘
```

## Graph DSL (JSON)

```json
{
  "id": "marketplace-default",
  "name": "Marketplace 标准分账",
  "version": "1.2.0",
  "triggers": [
    { "event": "charge.succeeded", "filter": "merchant.tier='marketplace'" }
  ],
  "nodes": [
    { "id": "src",        "type": "input",   "label": "顾客支付", "account_template": "platform_collected/{merchant_id}" },
    { "id": "platform",   "type": "account", "label": "平台手续费", "account_template": "platform_fee_revenue/{merchant_id}" },
    { "id": "seller",     "type": "account", "label": "卖家",       "account_template": "seller_balance/{seller_id}",     "from_attr": "seller_id" },
    { "id": "referrer",   "type": "account", "label": "推广员",    "account_template": "referrer_balance/{referrer}",     "from_attr": "referrer", "optional": true },
    { "id": "logistics",  "type": "account", "label": "物流方",    "account_template": "logistics_balance/{logistics_id}", "from_attr": "logistics_id", "optional": true }
  ],
  "edges": [
    { "from": "src", "to": "platform",  "rule": { "type": "percent", "value": 500  } },
    { "from": "src", "to": "referrer",  "rule": { "type": "percent", "value": 200, "if_missing": "skip" } },
    { "from": "src", "to": "logistics", "rule": { "type": "fixed_minor", "value": 500, "if_missing": "skip" } },
    { "from": "src", "to": "seller",    "rule": { "type": "remainder" } }
  ],
  "guards": [
    { "kind": "amount_min",  "value": 100,    "msg": "金额过小不分账" },
    { "kind": "merchant_active", "msg": "商户冻结禁止资金流" }
  ],
  "hold": { "days": 7, "applies_to": ["seller"] },
  "reversal": { "strategy": "proportional", "platform_covers_shortfall": true }
}
```

## Triggers — 事件驱动

| 事件 | 来源 | 典型 graph |
|---|---|---|
| `charge.succeeded` | order-core | marketplace 分账 / referral 返利 |
| `refund.completed` | refund-engine | 反向资金流 (按原比例退) |
| `hold.expired` | split-payment cron | 释放卖家保留期资金 |
| `dispute.lost` | dispute-service | 整笔金额从平台扣 |
| `payout.requested` | clearing-settlement | 卖家提现 → 清结算 |
| `subscription.cycle` | (新) subscription-service | 周期扣费分账 |

## Workflow Engine (调度)

```go
engine := workflow.New(workflow.Config{
    Trigger: kafka,   // 订阅业务事件
    GraphRepo: repo,  // 从 DB 拉 graph
    Translator: translator,  // graph → accounting batch
})
engine.RegisterHandler("charge.succeeded", handleChargeSucceeded)
engine.Run(ctx)
```

收到 `charge.succeeded`:
1. 找所有 `triggers[].event == "charge.succeeded"` 的 graph
2. 逐个 `evaluate(filter)` 看是否命中 (Starlark 表达式)
3. 命中的 graph 进 translator
4. Translator 把 graph + attributes 翻译成 `AtomicBatchBookingRequest`
5. accounting 原子下账, voucher_no 写回 plan 表

## 关键设计取舍

| 选项 | 决策 | 原因 |
|---|---|---|
| **执行原子性** | accounting `AtomicBatchBooking` 一次提交 | all-or-nothing, 不需要 saga |
| **图执行顺序** | 拓扑排序 + 余额累加 | 简单可推理, 不支持环 (避免无限) |
| **placeholder 注入** | trigger 事件 payload 直接进 attributes | 不另外查询业务 db, 解耦 |
| **保留期 (hold)** | 不真锁账, 在 accounting 用 sub-account `seller_balance_unsettled/{id}` | 利用 accounting 的多账户能力 |
| **回滚 (reverse)** | 按原 plan 反向 batch | 不需要双向边, 自动派生 |
| **图变更** | 改 graph 不影响历史 plan (每个 plan 存 graph_version) | 审计追溯到当时规则 |

## 实现路径

| 模块 | 状态 | 文件 |
|---|---|---|
| MoneyFlowGraph JSON schema | ✅ | `internal/domain/graph.go` |
| Graph translator | ✅ | `internal/workflow/translator.go` |
| Workflow engine (Kafka subscriber) | ✅ | `internal/workflow/engine.go` |
| accounting-system 客户端 | ✅ | `internal/clients/accounting.go` |
| Graph designer SPA | ✅ | `packages/payment-admin-web/frontend/moneyflow-designer.html` |
| Graph admin API | ✅ | `internal/adminhttp/graph.go` |
| 内置示例 graphs | ✅ | `examples/moneyflow-graphs/*.json` |

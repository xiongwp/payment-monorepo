# Subscription Service

商户 SaaS / 会员制 / 周期扣款引擎。

## 关键概念

| 概念 | 说明 |
|---|---|
| **Plan** | 订阅计划 (价格 / 周期 / 试用期 / 宽限期) |
| **Subscription** | 商户 × 用户的订阅实例 (状态机) |
| **Invoice** | 每周期生成的账单 (item line) |
| **Cycle** | 一次计费周期 (current_period_start/end) |
| **Dunning** | 失败重试链 (3次 7d 21d) |
| **Proration** | 升降级时按剩余天数补/退 |

## 状态机

```
   incomplete → trialing → active ─┬─→ canceled
                                    ├─→ past_due (dunning) ─→ canceled / unpaid
                                    └─→ ended (固定期数结束)
```

## 跟现有系统的集成

```
   subscription cron (每 5min)
        ↓
   找 "current_period_end < now" 的 active 订阅
        ↓
   创建 Invoice → 调 payment-gateway 收款
        ↓
   收款成功 → 发 "subscription.cycle" 事件
                                 ↓
              [Money Flow Graph engine]
                                 ↓
              graph "marketplace-subscription" 触发 → 分账走 accounting
        ↓
   收款失败 → dunning (3 次重试) → past_due → canceled
```

**重点**: 周期扣款产生的事件 (`subscription.cycle`) 走 **Money Flow Graph** 自动分账 — 复用刚做好的资金流编排，不需要 subscription 自己实现分账逻辑。

## 流程图

```
顾客订 $20/月会员
   ↓
[trial 7d]
   ↓
day 7: 首次扣款 $20 → succeeded → "subscription.cycle" 事件
                                       ↓
                                   [Graph: SaaS 标准分账]
                                       ↓
                                   平台留 30% / 服务商拿 70%
                                       ↓
                                   accounting 原子下账
day 37: 续费 $20 → 同上
   ...
day 67: 卡过期, 扣款 failed
   ↓
   dunning #1 (3 day) → 用户更新卡 / 重扣
   ↓ 仍失败
   dunning #2 (7 day) → 警告邮件
   ↓ 仍失败
   dunning #3 (14 day) → past_due → canceled
```

## 文件

```
packages/subscription/
├── README.md
├── go.mod
├── cmd/server/main.go              入口 + cron worker
├── internal/
│   ├── domain/                     Plan / Subscription / Invoice / DunningEvent
│   ├── workflow/
│   │   ├── lifecycle.go            状态机迁移
│   │   ├── cycle.go                周期扣款 worker
│   │   ├── dunning.go              重试链
│   │   └── proration.go            升降级按比例
│   ├── repo/memory.go              内存版仓储
│   ├── clients/
│   │   ├── gateway.go              调 payment-gateway 收款
│   │   └── moneyflow.go            发 subscription.cycle 事件 (→ moneyflow engine)
│   ├── adminhttp/server.go         HTTP CRUD
│   └── domain/types_test.go
└── examples/plans.json              示例 plans
```

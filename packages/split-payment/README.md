# Split Payment Service

把一笔商户收款按规则拆给多个收款方 — Marketplace / SaaS / 平台经济必备。

## 场景

```
顾客付 $100 →  平台 (master merchant)
                ↓
            拆分规则:
              80% → 卖家 (sub-merchant: seller_001)
              10% → 推广员 (referrer_xx)
              5%  → 平台手续费
              5%  → 物流方
```

## 关键概念

| 概念 | 说明 |
|---|---|
| **MasterMerchant** | 主商户, 接收交易 |
| **SubMerchant** | 子商户 / 收款方 / 推广员 |
| **SplitRule** | 拆分规则 (固定金额 / 百分比 / 阶梯) |
| **SplitPlan** | 一次交易的具体拆分方案 (实例化的 rule) |
| **SplitTransaction** | 实际记账条目 (一拆 N 条) |
| **HoldPeriod** | 留存期 (e.g. 卖家发货前钱压在平台 7d) |
| **Adjustment** | 退款 / 拒付时反向拆分 |

## 状态机

```
   created → calculated → executing → ┬── completed
                                       └── failed → ☢ DLQ
```

## 接入

```go
sp := splitpayment.New(splitpayment.Config{
    LedgerClient: accountingClient,
    AuditClient:  auditClient,
})

// 创建规则 (商户后台一次性配)
ruleID, _ := sp.CreateRule(ctx, &splitpayment.Rule{
    MerchantID: "mer_acme",
    Name:       "marketplace standard",
    Items: []splitpayment.RuleItem{
        {Beneficiary: "platform_fee", Type: "percent", Value: 5_00},  // 5%
        {Beneficiary: "{seller_id}",  Type: "percent", Value: 90_00, FromAttribute: "seller_id"},
        {Beneficiary: "{referrer}",   Type: "percent", Value: 5_00, Optional: true, FromAttribute: "referrer"},
    },
    HoldPeriodDays: 7,
})

// 交易完成时调用
plan, _ := sp.Execute(ctx, splitpayment.ExecuteRequest{
    ChargeID: "ch_001", AmountMinor: 10000, Currency: "USD",
    Attributes: map[string]string{"seller_id": "seller_001", "referrer": "ref_jane"},
    RuleID:   ruleID,
})
// plan.Items: [platform_fee:500, seller_001:9000, ref_jane:500]
// 都已经在 accounting-system 双账核入 ledger
```

## 退款流程

退款发起 → split-payment Reverse plan:
- 从 platform_fee / seller_001 / ref_jane 三个账户**按原比例**扣回
- 余额不足 (e.g. 卖家已提现) → 平台垫付 + 创建 receivable

## 实现 (见 internal/):

- `domain/`         — Rule / RuleItem / Plan / SplitTransaction
- `workflow/`       — Execute / Reverse / HoldRelease
- `repo/`           — MySQL + in-memory
- `clients/`        — accounting-system + audit-log 调用
- `cmd/server/`     — HTTP/gRPC entrypoint

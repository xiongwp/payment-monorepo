# SP-AC-7 端到端例子 — 用户充值 ¥100

跟着这个 case 把 graph → translator → gRPC → accounting-system 整条链路过一遍。

## 业务场景

用户 (user_id=42) 用支付宝充值人民币 100 元到平台:

- 渠道(支付宝)收手续费 ¥0.60 (60 分)
- 平台收手续费 ¥0.40 (40 分)
- 用户钱包最终到账 ¥99.00 (9900 分)
- 渠道结算 T+1,平台 fee 实时清算

期望的账务效果:

```
Phase 1 — channel.settled 事件 (3 条 leg 一次原子落账):
  借 PLATFORM_RECEIVABLE_CHANNEL/sub_42       10000
    贷 PLATFORM_CHANNEL_INBOUND_SUSPENSE/main 10000
  借 PLATFORM_CHANNEL_INBOUND_SUSPENSE/main    9900
    贷 USER_WALLET/42                          9900
  借 PLATFORM_CHANNEL_INBOUND_SUSPENSE/main     100
    贷 PLATFORM_FEE_CLEARING/main               100

Phase 2 — fee.cleared 事件 (2 条 leg 一次原子落账):
  借 PLATFORM_FEE_CLEARING/main                  60
    贷 CHANNEL_FEE_PAYABLE/alipay                60
  借 PLATFORM_FEE_CLEARING/main                  40
    贷 PLATFORM_FEE_REVENUE/main                 40
```

注意: 一个 event_code = 一次原子操作 = 多条 leg 一起成功或一起回滚。

---

## 第 1 步:Graph 配置 (`seed/scenarios/user_topup.graph.json`)

6 个 node, 5 条 edge, 每个 node 配一个 `account_id_attr` 告诉运行时去 attributes 哪个 key 取 account_id:

```json
{
  "key": "user-topup-default",
  "spec": {
    "scenario": "user_topup",
    "triggers": [{"event": "channel.settled"}],
    "guards": [{"kind": "amount_min", "value": 1, "msg": "充值金额必须 > 0"}],
    "nodes": [
      {"id": "channel_receivable", "type": "input",        "account_id_attr": "channel_receivable_account"},
      {"id": "suspense",           "type": "intermediate", "account_id_attr": "channel_suspense_account", "auto_clear": true},
      {"id": "user_wallet",        "type": "account",      "account_id_attr": "user_id_account"},
      {"id": "fee_clearing",       "type": "intermediate", "account_id_attr": "fee_clearing_account",   "auto_clear": true},
      {"id": "channel_payable",    "type": "output",       "account_id_attr": "channel_fee_account"},
      {"id": "platform_revenue",   "type": "output",       "account_id_attr": "fee_account"}
    ],
    "edges": [
      {"from": "channel_receivable", "to": "suspense",         "event_code": "channel_settled_receivable",   "rule": {"type": "remainder"}},
      {"from": "suspense",           "to": "user_wallet",      "event_code": "channel_settled_to_user",       "rule": {"type": "percent", "value": 9900}},
      {"from": "suspense",           "to": "fee_clearing",     "event_code": "channel_settled_fee_pending",   "rule": {"type": "remainder"}},
      {"from": "fee_clearing",       "to": "channel_payable",  "event_code": "fee_cleared_to_channel_payable","rule": {"type": "percent", "value": 6000}},
      {"from": "fee_clearing",       "to": "platform_revenue", "event_code": "fee_cleared_to_revenue",        "rule": {"type": "remainder"}}
    ]
  }
}
```

> 这个 seed 是 5 个 event_code 各 1 条 leg 的"细颗粒"版本,跟现有 SQL rule 一一对应。
> 真要享受 multi-leg 原子性,合并成 2 个 event_code 即可(`channel_settled` 3 legs / `fee_cleared` 2 legs),只是 SQL 那边的 rule schema 也要跟着支持多 leg。

---

## 第 2 步:Caller 触发 Graph

业务方(支付网关)收到支付宝结算回执后,组装 `TriggerContext` 调 split-payment engine:

```go
tc := workflow.TriggerContext{
    Event:       "channel.settled",
    ChargeID:    "topup_20260513_001",
    AmountMinor: 10000,  // 用于 guard 校验; 实际金额由 attributes 提供
    Currency:    "CNY",
    Attributes: map[string]string{
        // 每个 account_id_attr 配套 _amount / _currency 三件套
        "channel_receivable_account":          "PLATFORM_RECEIVABLE_CHANNEL/sub_42",
        "channel_receivable_account_amount":   "10000",
        "channel_receivable_account_currency": "CNY",

        "channel_suspense_account":          "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
        "channel_suspense_account_amount":   "10000",
        "channel_suspense_account_currency": "CNY",

        "user_id_account":          "USER_WALLET/42",
        "user_id_account_amount":   "9900",
        "user_id_account_currency": "CNY",

        "fee_clearing_account":          "PLATFORM_FEE_CLEARING/main",
        "fee_clearing_account_amount":   "100",
        "fee_clearing_account_currency": "CNY",

        "channel_fee_account":          "CHANNEL_FEE_PAYABLE/alipay",
        "channel_fee_account_amount":   "60",
        "channel_fee_account_currency": "CNY",

        "fee_account":          "PLATFORM_FEE_REVENUE/main",
        "fee_account_amount":   "40",
        "fee_account_currency": "CNY",
    },
    TraceID: "trace_abc123",
}
plan, err := workflow.Translate(graph, tc)
```

---

## 第 3 步:Translator 输出 — 5 个 TransactionRequest

每个 event_code 一个 TransactionRequest(因为 seed 是细颗粒 1-leg-per-event):

```json
[
  {
    "order_no":     "topup_20260513_001_channel_settled_receivable",
    "business_no":  "topup_20260513_001",
    "business_type":"user_topup",
    "product_code": "user_topup",
    "event_code":   "channel_settled_receivable",
    "legs": [
      {
        "edge_from_node":   "channel_receivable",
        "edge_to_node":     "suspense",
        "from_account_id": "PLATFORM_RECEIVABLE_CHANNEL/sub_42",
        "to_account_id":   "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
        "amount":           "10000",
        "currency":         "CNY"
      }
    ],
    "status":     "pending",
    "trace_id":   "trace_abc123",
    "created_at": "2026-05-13T08:00:00Z"
  },
  {
    "order_no":     "topup_20260513_001_channel_settled_to_user",
    "event_code":   "channel_settled_to_user",
    "legs": [{
      "edge_from_node":  "suspense",
      "edge_to_node":    "user_wallet",
      "from_account_id":"PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
      "to_account_id":  "USER_WALLET/42",
      "amount":          "9900",
      "currency":        "CNY"
    }],
    "...": "其他字段同上"
  },
  {
    "event_code": "channel_settled_fee_pending",
    "legs": [{"from_account_id": "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
              "to_account_id":   "PLATFORM_FEE_CLEARING/main",
              "amount":          "100",
              "currency":        "CNY"}]
  },
  {
    "event_code": "fee_cleared_to_channel_payable",
    "legs": [{"from_account_id": "PLATFORM_FEE_CLEARING/main",
              "to_account_id":   "CHANNEL_FEE_PAYABLE/alipay",
              "amount":          "60",
              "currency":        "CNY"}]
  },
  {
    "event_code": "fee_cleared_to_revenue",
    "legs": [{"from_account_id": "PLATFORM_FEE_CLEARING/main",
              "to_account_id":   "PLATFORM_FEE_REVENUE/main",
              "amount":          "40",
              "currency":        "CNY"}]
  }
]
```

### 如果合并成 2 个 event_code

如果你把 graph 改成 3 条 edge 共享 `event_code: "channel_settled"`、2 条 共享 `event_code: "fee_cleared"`,translator 会输出 2 个 TransactionRequest:

```json
[
  {
    "event_code": "channel_settled",
    "legs": [
      {"edge_from_node":"channel_receivable","edge_to_node":"suspense",     "amount":"10000","currency":"CNY","from_account_id":"...","to_account_id":"..."},
      {"edge_from_node":"suspense",          "edge_to_node":"user_wallet",  "amount":"9900", "currency":"CNY","from_account_id":"...","to_account_id":"..."},
      {"edge_from_node":"suspense",          "edge_to_node":"fee_clearing", "amount":"100",  "currency":"CNY","from_account_id":"...","to_account_id":"..."}
    ]
  },
  {
    "event_code": "fee_cleared",
    "legs": [
      {"edge_from_node":"fee_clearing","edge_to_node":"channel_payable",  "amount":"60","currency":"CNY"},
      {"edge_from_node":"fee_clearing","edge_to_node":"platform_revenue","amount":"40","currency":"CNY"}
    ]
  }
]
```

3 条 leg 在一个 TransactionRequest 里 → accounting-system 一次 DoubleEntryBooking → 一个 voucher_no → 要么全成要么全回滚。这才是真正的原子性收益。

---

## 第 4 步:engine 调 gRPC TransactionService

```
split-payment engine
  ↓ for each TransactionRequest in plan.Transactions:
  ↓
gRPC /accounting.v1.TransactionService/CreateTransaction
  ↓
accounting-system internal/grpc/transaction_handlers.go:CreateTransaction
  ↓
service.TransactionService.CreateTransaction
  ↓ (查 rule + 幂等 + CAS 抢锁)
  ↓
service.executeBookkeeping → 每条 leg 拆 2 个 entry (借 from, 贷 to)
  ↓
accountingService.DoubleEntryBooking
  ↓
  原子写入 transaction + account.balance + 生成 voucher_no
```

---

## 第 5 步:accounting-system 落账细节

以 `event_code: channel_settled_to_user` 这一笔为例(单 leg):

```go
// gRPC request 进来
req := &CreateTransactionRequest{
    OrderNo:     "topup_20260513_001_channel_settled_to_user",
    ProductCode: "user_topup",
    EventCode:   "channel_settled_to_user",
    Legs: []*TxnLeg{
        {
            FromAccountId: "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main",
            ToAccountId:   "USER_WALLET/42",
            Amount:        "9900",
            Currency:      "CNY",
        },
    },
}

// transaction_service.executeBookkeeping 把 leg 拆成借贷 entry
entries := []AccountingEntry{
    {AccountNo: "PLATFORM_CHANNEL_INBOUND_SUSPENSE/main", DebitAmount:  9900},
    {AccountNo: "USER_WALLET/42",                          CreditAmount: 9900},
}

// DoubleEntryBooking 一次原子写
voucherNo := "V_2026051308_xyz"
// → INSERT transaction (2 行, 同 voucher_no)
// → UPDATE account.balance ×2
// → 全部走同一事务
```

返回:

```json
{
  "code": 0,
  "order_no":   "topup_20260513_001_channel_settled_to_user",
  "status":     2,
  "voucher_no": "V_2026051308_xyz"
}
```

---

## 第 6 步:幂等重放

同 `order_no` 再调一次,accounting-system 直接返历史结果,不重复落账:

```go
resp1, _ := txnSvc.CreateTransaction(ctx, req)
// → 落账, voucher=V_2026051308_xyz

resp2, _ := txnSvc.CreateTransaction(ctx, req)
// → 走 GetByOrderKey 命中 SUCCESS → 直接返 voucher=V_2026051308_xyz (no-op)
```

CAS 抢锁逻辑保证并发同 order_no 只有一个实例真正执行,其他实例看到 PROCESSING 状态直接返回。

---

## 第 7 步:重试失败订单

第一次失败(比如 accounting-system 临时不可用):

```go
resp, _ := txnSvc.CreateTransaction(ctx, req)
// resp.Status = 3 (failed)
// resp.ErrorMessage = "..."
```

订单已存在但 Status=Failed,RetryCount=1,Extra 里持久化了完整 Legs。手动或自动重试:

```go
resp, _ := txnSvc.RetryTransaction(ctx, "topup_20260513_001_channel_settled_to_user")
// 从 TransactionOrder.Extra 还原 Legs,重新跑 executeBookkeeping
```

最大 RetryCount(默认 3)用完后 RetryTransaction 拒绝再试,需要人工介入。

---

## Designer 怎么画这个 Graph

在 `/moneyflow/designer` 拖出 6 个节点,每个节点的 panel 填三件套(只填 Account ID attr 就够,Amount/Currency attr 留空走默认 convention `<aid>_amount` / `<aid>_currency`):

```
[input]   channel_receivable    →  account_id_attr: channel_receivable_account
[interm]  suspense              →  account_id_attr: channel_suspense_account   ☑ auto_clear
[acct]    user_wallet           →  account_id_attr: user_id_account
[interm]  fee_clearing          →  account_id_attr: fee_clearing_account       ☑ auto_clear
[output]  channel_payable       →  account_id_attr: channel_fee_account
[output]  platform_revenue      →  account_id_attr: fee_account
```

边连完后,每条 edge 的 panel 填 `event_code` 名字(在 v2 designer 里有专门的输入框),想合并几条 edge 成一次原子操作,就给它们配相同的 event_code。

---

## 一句话总结

```
caller event/request payload  ←—  按 account_id_attr 配三件套 (id/amount/currency)
        │
        ▼
split-payment translator      ←—  按 event_code 分组 → 每组 1 个 multi-leg TransactionRequest
        │
        ▼ gRPC /accounting.v1.TransactionService/CreateTransaction
        ▼
accounting-system             ←—  查 rule 校验存在 + 每条 leg 拆借贷 entry + DoubleEntryBooking 原子落账
```

每个 event_code = 一个 voucher_no = 一次原子操作。

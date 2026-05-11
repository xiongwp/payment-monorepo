# Wallet Service

用户/商户多币种储值账户 — 跟 accounting 之间是一层"业务包装"。

## 定位

```
                    用户视角                     accounting 视角
                  ─────────────                ─────────────────
   "我有 1000 USD + 5000 JPY 在钱包里"  ←→  cust_wallet/u123/USD: 1000_00
                                              cust_wallet/u123/JPY: 5000_00
   "充值 100 USD"                       ←→   AtomicBatch:
                                              DEBIT  source_card_payable
                                              CREDIT cust_wallet/u123/USD
   "支付 50 USD 给商户"                  ←→   AtomicBatch:
                                              DEBIT  cust_wallet/u123/USD
                                              CREDIT mer_collected/m1
```

Wallet **不**重新发明账户系统 — 它就是一组规范的 account_id 命名 +
高层 API (Topup / Pay / Withdraw / Transfer)。

## API

```
POST /wallets                          创建钱包 (默认 USD)
GET  /wallets/{owner}/balances         查所有币种余额
POST /wallets/{owner}/topup            充值 (从绑定支付方式)
POST /wallets/{owner}/pay              支付到商户 (内部转账, 0 手续费)
POST /wallets/{owner}/withdraw         提现 (到绑定银行账户, 进 clearing 排队)
POST /wallets/{owner}/transfer         钱包间转账
POST /wallets/{owner}/convert          内部 FX 兑换 (调 fx-service)
GET  /wallets/{owner}/transactions     交易历史
POST /wallets/{owner}/freeze           冻结 (合规 / 异常)
POST /wallets/{owner}/unfreeze
```

## 跟现有系统

| 流程 | Wallet 调用 |
|---|---|
| Topup | payment-gateway (生成 charge) → 成功后 accounting 双账 |
| Pay | accounting AtomicBatch (内部 0 手续费) |
| Withdraw | clearing-settlement.Payout (排队 + 银行 ACH) |
| FX 兑换 | fx-service.Quote + Confirm |
| 冻结 | accounting.FreezeAccount + audit log |

## 状态约束

| Invariant | 检查 |
|---|---|
| 余额不可负 (除信用账户) | accounting 内部 NotEnoughBalance 错 |
| 提现金额 ≤ 当前余额 - 锁定 | wallet 端检查 + accounting 复验 |
| 冻结状态不能 pay/withdraw | wallet API 层先拦, accounting 也拦 |
| 提现到 3rd party 必 KYC pass | 调 kyc-service 检查 |

## 文件

```
packages/wallet-service/
├── README.md
├── go.mod
├── cmd/server/main.go
├── internal/
│   ├── domain/types.go         Wallet/Balance/Transaction
│   ├── workflow/
│   │   ├── topup.go            充值 (gateway → accounting)
│   │   ├── pay.go              支付 (accounting AtomicBatch)
│   │   ├── withdraw.go         提现 (clearing 排队)
│   │   └── transfer.go         钱包间转账
│   ├── repo/memory.go
│   ├── clients/
│   │   ├── accounting.go
│   │   ├── gateway.go
│   │   ├── clearing.go
│   │   └── fx.go
│   └── adminhttp/server.go
└── examples/wallet-flows.md
```

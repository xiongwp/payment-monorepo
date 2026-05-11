# FX Service — 多币种 + 汇率管理

跨境支付平台必备 — 任何 multi-region / multi-currency 业务都靠它。

## 职责

| 职责 | 说明 |
|---|---|
| **Rate ingestion** | 多源拉汇率: ECB / OANDA / 渠道 / 商户自定义; 每分钟入库 |
| **Rate query** | 当前 mid / bid / ask 三档; 历史 (按时间) |
| **Currency conversion** | 1 USD → JPY = N (买入/卖出 spread + 平台手续费) |
| **Multi-currency wallet** | 同一账户多币种余额, accounting 已支持 |
| **Hedging hint** | 当某币种敞口 > 阈值告警 (财务对冲) |

## 数据流

```
   多源 rate fetcher (cron 每 60s)
     ECB / OANDA / Stripe FX / 内部 manual
              ↓
        fx_rates 表 (currency_pair, source, mid/bid/ask, fetched_at)
              ↓
   ┌──────────┴──────────┐
   │                     │
 API: GET /rates       conversion engine
                         ↓
                   accounting (账户多币种) + payment-gateway (定价)
```

## 关键设计

| 决策 | 原因 |
|---|---|
| **数据源多 + 加权** | 单源故障不会断 (ECB 周末没数据 → 用 OANDA) |
| **存 bid/ask 不存 spread** | spread 由商户/产品定义, 不写死 |
| **base = USD** | 简化 — 任何币种通过 USD 转 (USD↔JPY + USD↔EUR ⇒ JPY↔EUR) |
| **rate scale 8 位小数** | 防丢精度 (e.g. 1 USD = 153.45678901 JPY); 用整数 * 1e8 存 |
| **TTL** | 默认 5min; 商户可指定 LOCKED 锁价 (购买时) |
| **历史** | 永久保留, 90 天后 ClickHouse 冷存 |

## API

```
POST /fx/quote
  { "from": "USD", "to": "JPY", "amount_minor": 10000 }
  →
  { "rate": "153.4568", "amount_to_minor": 1534568,
    "quote_id": "q_xxx", "expires_at": "2026-05-11T...", "fee_minor": 50 }

POST /fx/confirm
  { "quote_id": "q_xxx" }
  → accounting 落账: USD 账户 - 100 / JPY 账户 + 1534.568 / fee 账户 + 0.50

GET  /fx/rates?from=USD&to=JPY     当前 mid (默认源, 5min cache)
GET  /fx/rates?from=USD&to=JPY&at=2026-05-11T00:00:00Z    历史 (snapshot)
GET  /fx/exposure?merchant_id=m1   敞口分析 — 平台 / 商户级
POST /fx/admin/sources             admin: 添加 / 启停数据源
```

## 落账 (accounting 多币种)

每个 conversion 翻译成 2 条原子记账:
```
  DEBIT  cust_usd_wallet/u123    100.00 USD
  CREDIT cust_jpy_wallet/u123    15345.68 JPY
  + fee_revenue                  0.50 USD
```

accounting-system 的 AccountingEntry 已支持 currency 字段; FX 服务只需把
**两条不同币种的分录打包**进一个 `AtomicBatchBookingRequest` 即可。

## 文件

```
packages/fx-service/
├── README.md
├── go.mod
├── cmd/server/main.go
├── internal/
│   ├── domain/                 Rate / Quote / Conversion
│   ├── repo/memory.go
│   ├── sources/                ECB / OANDA / manual / stripe sources
│   ├── workflow/
│   │   ├── quote.go            报价 + 锁价
│   │   ├── convert.go          执行兑换 → accounting
│   │   └── exposure.go         敞口分析
│   ├── clients/accounting.go
│   └── adminhttp/server.go
└── examples/sources.json
```

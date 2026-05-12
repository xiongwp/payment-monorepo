# tax-reporting

商户税务申报服务. 把全平台 payout 流水滚出 1099-K / W-9 / W-8 / VAT OSS 表 + e-file.

## 为什么需要

- **US 1099-K**: IRS 2026 阈值 \$600 ⇒ 几乎所有商户都触发, 不申报 \$290/份 罚
- **EU VAT OSS**: €10k 跨境就要月报, 不报失去欧盟市场
- **W-9 / W-8**: 商户拿钱前的法定身份采集, 不收不能放款

## 流程

```
clearing-settlement / billing-system
            │ POST /v1/payouts (持续推 PayoutEvent)
            ▼
┌────────────────────────────────────────────┐
│  tax-reporting                             │
│  ┌──────────────────────────────────────┐  │
│  │ ingest (PayoutEvent → store)         │  │
│  └──────────────────────────────────────┘  │
│  ┌──────────────────────────────────────┐  │
│  │ aggregator (incremental + recompute) │  │
│  │   (merchant, year, jurisdiction)     │  │
│  │   → monthly + total                  │  │
│  └──────────────────────────────────────┘  │
│  ┌──────────────────────────────────────┐  │
│  │ profile (W-9 / W-8 / VAT 收集)       │  │
│  └──────────────────────────────────────┘  │
│              │ year-end cron                 │
│              ▼                                │
│  ┌──────────────────────────────────────┐  │
│  │ forms generate (1099-K / VAT OSS)    │  │
│  └──────────────────────────────────────┘  │
│              │                                │
│              ▼                                │
│  ┌──────────────────────────────────────┐  │
│  │ efile.Submitter                      │  │
│  │   stub | IRS FIRE | Avalara          │  │
│  └──────────────────────────────────────┘  │
└────────────────────────────────────────────┘
```

## API

### 业务

| 路由 | 用途 |
|---|---|
| `POST /v1/payouts` | 推 PayoutEvent (idempotent by event_id) |
| `POST /v1/profile` | 商户 W-9 / W-8 / VAT 信息收集 |
| `GET /v1/aggregate/{mid}/{year}/{juris}` | 拿年度汇总 |
| `POST /v1/forms/1099k/{mid}/{year}` | 生成 1099-K |
| `POST /v1/forms/{form_id}/file` | e-file 提交 |
| `GET /v1/forms/{form_id}` | 拿表详情 (含 payload) |

### 商户自助

| 路由 | 用途 |
|---|---|
| `GET /v1/me/forms` | 商户拉自己历年税表 |
| `GET /v1/me/forms/{form_id}` | 下载明细 |

### Admin

| 路由 | 用途 |
|---|---|
| `GET /admin/eligible/{year}` | 触发阈值的商户列表 |
| `POST /admin/bulk-generate/{year}` | 批量生成 (cron) |

## 阈值表 (DefaultThresholds)

| 地区 | 表 | 年 | gross 阈值 |
|---|---|---|---|
| US | 1099-K | 2024 | \$5000 |
| US | 1099-K | 2025 | \$2500 |
| US | 1099-K | **2026+** | **\$600** |
| EU | VAT OSS | 2025+ | €10,000 |
| GB | UK VAT MTD | 2026+ | £90,000 |

## e-file provider

- **stub** (dev): 立即返已"接受"
- **IRS FIRE** (prod, 自建): TCC 注册 + Pub 1220 fixed-width upload, SFTP
- **Avalara** (prod, 商用): REST API, Bearer auth, JSON 上送

`TAX_EFILE_PROVIDER` 切换 (默认 stub).

## 集成

```go
// clearing-settlement, payout 完成后:
client.Post(taxReportingBase+"/v1/payouts", PayoutEvent{
    EventID:    "payout_"+payoutID,
    MerchantID: merchantID,
    OccurredAt: settledAt,
    GrossAmount: gross,
    NetAmount:   net,
    FeesAmount:  fees,
    Currency:   "USD",
    Jurisdiction: "US",
    TxnCount:   1,
    Source:     "clearing",
    Channel:    "visa",
})
```

## 部署

```bash
cd ../payment-admin-web
./deploy.sh up tax-reporting
curl http://localhost:18090/healthz
```

## 不在 scope (下一步)

- 真 IRS FIRE / Avalara HTTPS transport
- PDF 渲染 (gotenberg / weasyprint)
- 多年度 amended 申报 (1099-K corrections)
- 商户自助 W-9 数字签名 (HelloSign / DocuSign 接入)
- 加州 / 纽约 州税同时报送

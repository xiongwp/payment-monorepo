# aml-screening

AML / 制裁 / PEP 实时筛查服务. 商户入网 (KYB)、大额出款、跨境支付的合规拦截层.

## 架构

```
business svc (kyc / clearing-settlement / payout)
        │ POST /v1/screen
        ▼
┌────────────────────────────────┐
│ aml-screening                  │
│  ┌──────────────────────────┐  │
│  │ /v1/screen (sync)        │  │
│  │  ↓                        │  │
│  │ candidate store (idx)    │──┼── 70w+ entries 内存索引
│  │  ↓                        │  │
│  │ matcher (Jaro-Winkler)   │  │
│  │  ↓                        │  │
│  │ Decide → block/review/pass│  │
│  └──────────────────────────┘  │
│  ┌──────────────────────────┐  │
│  │ Refresher (daily)        │  │
│  │  OFAC SDN  /  EU cons    │──┼── HTTP/SFTP fetch
│  │  UK HMT    /  UN SC      │  │
│  │  PEP db                  │  │
│  └──────────────────────────┘  │
│  ┌──────────────────────────┐  │
│  │ admin: hits 复核工作流   │  │
│  └──────────────────────────┘  │
└────────────────────────────────┘
        │
        ▼
   audit-log
```

## API

### 业务

| 路由 | 用途 |
|---|---|
| `POST /v1/screen` | 同步筛查 (返回 action: pass/review/block) |
| `GET /v1/screen/{request_id}` | 查历史结果 (幂等) |

### Admin (X-Admin-Token)

| 路由 | 用途 |
|---|---|
| `GET /admin/hits/pending` | 待复核命中 |
| `POST /admin/hits/{hit_id}/resolve` | ops 复核决策 (cleared / frozen / escalated) |
| `GET /admin/lists/{source}/count` | 名单条目数 |
| `POST /admin/lists/{source}/refresh` | 立即拉名单 |

## 匹配算法

**6 步标准化**:
1. Unicode 重音去除 (José → Jose)
2. lowercase
3. 公司后缀剥离 (Inc / Ltd / GmbH / 有限公司 …)
4. 标点折叠 + 空格归一
5. 词序无关 (alpha sort)
6. 国家代码 → ISO 3166-1 alpha-2

**评分 (0-100)**:
- name primary 强匹配 (>0.92) → +60
- alias 强匹配 → +55
- DOB 完全 → +20 (年份匹配 → +10)
- nationality → +10
- ID hash 命中 → +90 (单字段几乎满分)
- address strong → +10

**阈值**:
- ≥ 90 → `block`
- ≥ 70 → `review`
- < 70 → `pass`

## 部署

```bash
make image                          # 镜像
docker compose up -d                # 起单服务 (memory store, dev seed)
make test                           # 单测
bash test/smoke.sh                  # 烟雾
```

或走 payment-admin-web 联栈:

```bash
cd ../payment-admin-web
./deploy.sh up aml-screening
curl http://localhost:18088/healthz
```

## 名单源接入

| 源 | URL | 频率 | env |
|---|---|---|---|
| OFAC SDN | treasury.gov/ofac/downloads/sdn.xml | 每日 | `AML_REFRESH_OFAC=1` |
| EU Consolidated | webgate.ec.europa.eu/europeaid/fsd | 每日 | `AML_REFRESH_EU=1` (TODO) |
| UK HMT | hmrc.gov.uk/SanctionsList.csv | 每日 | (TODO) |
| UN SC | un.org/securitycouncil/.../consolidated | 每周 | (TODO) |
| PEP / Adverse Media | 商用 (ComplyAdvantage / Refinitiv) | 实时 | (TODO) |

dev 默认 `AML_DEV_SEED=1`, 灌 4 条已知名单 (John Doe / Acme / Vladimir Testov / Hassan Politico).

## 集成例子

KYB onboarding:

```go
req := AMLScreenRequest{
    RequestID:  "kyb-" + merchantID,
    Trigger:    "kyb_onboarding",
    Subject:    "entity",
    Name:       merchant.LegalName,
    Nationality: merchant.IncorporatedCountry,
    Addresses:  []string{merchant.RegisteredAddress},
    MerchantID: merchantID,
}
res, _ := amlClient.Screen(ctx, req)
switch res.Action {
case "block":
    return KYBReject(res.Hits[0].ListEntry.Program)
case "review":
    return KYBPending() // ops 在 biz-admin-web 复核
case "pass":
    return KYBAutoApprove()
}
```

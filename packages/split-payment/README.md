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

## mTLS (SP-AC-7 PH3-2)

split-payment 的 gRPC 服务端 + accounting client 都接 `payment-util/mtls`,
通过环境变量打开双向认证.

环境变量:

```
ENVIRONMENT=production       # prod/production 时强制 mTLS, cert 缺失 fail-fast
MTLS_SERVER_CERT=/etc/certs/server.crt
MTLS_SERVER_KEY=/etc/certs/server.key
MTLS_CA_CERT=/etc/certs/ca.crt
INSECURE_DIAL=1              # dev only — 跳过 mTLS 走明文 (prod 自动忽略)
```

行为:

- Server: 配齐 → `RequireAndVerifyClientCert`, 拒绝无 client cert 的 dial;
  未配 → log warn + 明文 (dev 模式).
- Client: 配齐 → 同时校验 server cert + 出示 client cert; 未配 + 非 prod → 明文.
- Prod 模式 (`ENVIRONMENT=prod` 或 `production`) 缺证书直接进程退出, 避免裸跑.

K8s 接入:

- `deploy/k8s/cert-manager-cert.yaml` — cert-manager `Certificate` 自动签发到
  Secret `split-payment-mtls`, 90 天 lifetime / 30 天前自动 renew.
- `deploy/k8s/deployment.yaml` — Pod 挂 Secret 为 volume `/etc/certs/{server.crt,server.key,ca.crt}`.
- 同 PKI 给 accounting-system 签一张证书 (issuer 同 ClusterIssuer payment-internal-ca),
  两边 trust 同一 CA 即可互通.

证书 rotation 时:

- cert-manager 自动重生 Secret; kubelet 通过 projected volume 在 60s 内热更新 Pod 文件.
- split-payment 进程不需要重启 — Go 的 `tls.Config.GetCertificate` 默认 once-loaded;
  当前实现也是一次性加载 (启动期 `tls.LoadX509KeyPair`), rotation 后下一次 dial / handshake
  自然就用新证. 老连接复用旧证直到 keepalive 断开 + 重连.

参考: `packages/payment-util/mtls/MTLS.md`.

测试

curl -sS -X POST http://localhost:19190/api/moneyflow/trigger \
  -H 'Content-Type: application/json' \
  -d '{
    "graph_key":"user-topup-multileg",
    "event":{
      "event":"channel.settled","charge_id":"topup_demo_100000100_007",
      "amount_minor":10000,"currency":"PHP",
      "attributes":{
        "channel_receivable_account":"PLATFORM_RECEIVABLE_CHANNEL/100000100",
        "channel_receivable_account_amount":"10000","channel_receivable_account_currency":"PHP",
        "channel_suspense_account":"PLATFORM_CHANNEL_INBOUND_SUSPENSE/100000100",
        "channel_suspense_account_amount":"10000","channel_suspense_account_currency":"PHP",
        "user_id_account":"USER_BALANCE/100000100",
        "user_id_account_amount":"9900","user_id_account_currency":"PHP",
        "fee_clearing_account":"PLATFORM_FEE_CLEARING/100000100",
        "fee_clearing_account_amount":"100","fee_clearing_account_currency":"PHP",
        "channel_fee_account":"CHANNEL_FEE_PAYABLE/100000100",
        "channel_fee_account_amount":"60","channel_fee_account_currency":"PHP",
        "fee_account":"PLATFORM_FEE_REVENUE/100000100",
        "fee_account_amount":"40","fee_account_currency":"PHP"
      }
    }
  }' | jq '.data.vouchers'
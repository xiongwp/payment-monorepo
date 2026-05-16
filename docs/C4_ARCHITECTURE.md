# C4 Architecture Diagram — Payment Platform

C1-3 视角的 Mermaid 图。生产可用 PlantUML / Structurizr 重画;Mermaid 足够 README 嵌入。

## C1 — System Context

```mermaid
C4Context
    title 支付平台系统上下文

    Person(merchant, "Merchant", "接入 SDK 收款的商户")
    Person(customer, "Customer", "C 端付款用户")
    Person(ops,      "Ops / SRE", "运维 / 合规 / 财务")

    System(payment, "Payment Platform", "Charge / Refund / Settlement / Dispute / Recon")

    System_Ext(visa,     "Visa Net",       "卡组网络")
    System_Ext(mastercard,"Mastercard",    "卡组网络")
    System_Ext(jcb,      "JCB",            "卡组网络")
    System_Ext(banks,    "Acquirer Banks", "收单行 / 发卡行")
    System_Ext(fx,       "FX Provider",    "Reuters / OANDA 汇率")
    System_Ext(kyc,      "KYC Provider",   "Sumsub / Veriff")
    System_Ext(aml_src,  "AML Lists",      "OFAC / EU sanctions")

    Rel(customer, payment, "支付")
    Rel(merchant, payment, "API / SDK 接入")
    Rel(ops, payment, "Admin / 监控")
    Rel(payment, visa, "Authorize / Capture (mTLS)")
    Rel(payment, mastercard, "Authorize / Capture (mTLS)")
    Rel(payment, jcb, "Authorize / Capture (mTLS)")
    Rel(payment, banks, "Settlement / ACH / SWIFT")
    Rel(payment, fx, "Realtime FX Rate")
    Rel(payment, kyc, "Identity Verify")
    Rel(payment, aml_src, "Watchlist sync")
```

## C2 — Containers (Microservice 视角,38 个 packages)

```mermaid
flowchart LR
    subgraph Edge
        APIGW[api-gateway]
        ADMIN[biz-admin-web / payment-admin-web]
        WEBHOOK[merchant-webhook]
    end

    subgraph Core
        PGW[payment-gateway]
        PCORE[payment-core]
        OCORE[order-core]
        ACC[accounting-system]
        WALLET[wallet-service]
    end

    subgraph Channel
        PCHAN[payment-channel]
        CCEN[card-center]
        CPAY[card-payment]
        FX[fx-service]
    end

    subgraph Risk
        RISK[risk-manage]
        AML[aml-screening]
        KYC[kyc-service]
        DISP[dispute-service]
    end

    subgraph Settlement
        BILL[billing-system]
        CLR[clearing-settlement]
        REF[refund-engine]
        SUB[subscription]
        SPLIT[split-payment]
    end

    subgraph Platform
        OAUTH[oauth2-server]
        CFG[config-center]
        IDGEN[id-generator]
        AUDIT[audit-log]
        APPROV[approval-service]
        KMS[kms-manage]
        TOKVAULT[tokenization-vault]
    end

    subgraph Data
        MYSQL[(MySQL Primary+Replica)]
        REDIS[(Redis Sentinel)]
        KAFKA[[Kafka 3-broker]]
        CH[(ClickHouse)]
    end

    APIGW -->|JWT/mTLS| PGW
    APIGW --> ADMIN
    PGW --> PCORE
    PGW --> OCORE
    PCORE --> PCHAN
    PCORE --> RISK
    PCHAN --> CCEN
    CCEN --> CPAY
    CPAY -.->|Authorize| Visa
    OCORE --> ACC
    OCORE --> CCEN
    ACC --> WALLET
    REF --> ACC
    BILL --> ACC
    CLR --> WALLET
    SPLIT --> ACC
    DISP --> OCORE
    AML --> RISK
    KYC --> RISK

    PCORE --> MYSQL
    OCORE --> MYSQL
    ACC --> MYSQL
    PCHAN --> MYSQL
    RISK --> REDIS
    APIGW --> REDIS
    PCORE --> KAFKA
    OCORE --> KAFKA
    WEBHOOK --> KAFKA
    AUDIT --> CH
    BILL --> CH
```

## C3 — Component (payment-core 内部)

```mermaid
flowchart TB
    subgraph payment-core
        SVC[PaymentService]
        ROUTE[Router + FallbackRouter]
        BREAK[CircuitBreaker]
        RETRY_Q[DBRetryQueue + RetryWorker]
        OUTBOX[OutboxHook]
        SAGA[SagaCoordinator]
        KMS_CLI[KMSClient]
        CHAN_CLI[PaymentChannelClient gRPC]
        RISK_CLI[RiskClient gRPC]
    end

    SVC --> ROUTE
    ROUTE --> CHAN_CLI
    SVC --> RISK_CLI
    SVC --> KMS_CLI
    SVC --> OUTBOX
    SVC --> SAGA
    ROUTE --> BREAK
    BREAK -. failure .-> RETRY_Q
    RETRY_Q -. dequeue .-> SVC
```

## 关键不变量 (across the system)

1. **资金安全**: 所有 ledger 写都走 outbox + double-entry; 日切前必平 (trial-balance cron)
2. **幂等**: 每个 mutating endpoint 都接 Idempotency-Key,UNIQUE 索引兜底
3. **PCI scope 收敛**: PAN 只流过 card-center + card-payment 两个进程,其它服务一律拿 token
4. **mTLS**: 所有跨服务 gRPC 都强制 mTLS (除 dev/staging insecure flag);CN 白名单严格
5. **可观测**: 所有 entrypoint 注入 trace_id; payment-core / accounting-system 10% sampling, error 100%

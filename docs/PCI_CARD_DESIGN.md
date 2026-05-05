# PCI 卡数据架构（card-center + card-payment）

跟卡相关的资金流（用户存卡、付卡、退款）独立成 2 个服务部署到隔离 DC，
其它任何服务（含 payment-channel 主体）永远不见 PAN。

## 1. 服务分工

```
┌─────────────────────────── Main DC (SAQ-A-EP scope) ──────────────────────────┐
│  api-gateway → payment-core → payment-channel → order-core → user-merchant    │
│                                                                                │
│  这些服务永远只见 token，不见 PAN                                                │
└────────────────────────────────────────────────┬──────────────────────────────┘
                                                 │ mTLS over HTTPS
                                                 ↓
┌──────────────────────── Card DC (SAQ-D scope, 隔离) ─────────────────────────┐
│                                                                                │
│   ┌──────────── card-center ────────────┐    ┌──────── card-payment ──────┐   │
│   │  无数据库（纯加密 token）            │    │  调卡组织专用                │   │
│   │  KMS-backed envelope encryption     │←──→│  Visa / MC / JCB / AMEX     │   │
│   │  Tokenize / Detokenize APIs         │    │  唯一会拿 PAN 的服务          │   │
│   │  存卡 token + 一次性支付 token        │    │  仅 RPC 栈内存，调完即弃      │   │
│   └─────────────────────────────────────┘    └─────────────────────────────┘   │
│                                                          │                      │
│                                                          ↓ HTTPS + 专线/VPN     │
│                                                  ┌───────────────┐              │
│                                                  │  Visa Net /    │              │
│                                                  │  Mastercard MIP│              │
│                                                  │  JCB / AMEX    │              │
│                                                  └───────────────┘              │
└────────────────────────────────────────────────────────────────────────────────┘
```

## 2. card-center：纯加密 token 设计（无数据库）

### 2.1 为什么无数据库

传统 token vault 维护 PAN ↔ token 映射表。用 KMS-based encryption 后映射不需要存：
**token 本身就是 PAN 的密文**。Decrypt 即可还原 PAN。

优点：
- 无数据库 = 无数据泄露面
- 多副本无状态部署，水平扩容不需要分库分表
- 备份只需备份 KMS（已经做了）
- "删除"靠 KMS key rotation 周期性废弃，不需要 DELETE 操作

代价：
- "立即删除某张卡"做不到物理删除，只能业务层 soft delete + 等 key rotation 自然失效
- token 长度比传统 lookup token 长（~200 字节 vs 12 字节）

### 2.2 Token 类型

**存卡 token** (long-lived)：
```
tok_card_<base64url(envelope)>

envelope = KMS.Encrypt(
    plaintext = JSON{pan, exp_month, exp_year, holder_name, kid, iat},
    AAD = "card:user_id:<user_id>"   // 防止 token 被偷接给别人
)
```
- 跟 user_id 绑死（AAD 校验）
- 没有 TTL（只要 KMS key 还在就有效）

**一次性支付 token** (TTL 30min)：
```
tok_pay_<base64url(envelope)>

envelope = KMS.Encrypt(
    plaintext = JSON{pan, pi_id, exp_ts, nonce, kid, iat},
    AAD = "pay:pi:<pi_id>"
)
```
- 跟 pi_id 绑死
- exp_ts 内嵌，校验时检查 now < exp_ts
- "一次性"靠 PI 状态机保证（同一 PI 不会成功两次）+ AAD 防转用其它 PI

### 2.3 API（gRPC over mTLS）

```proto
service CardCenter {
  // 用户存卡入口
  rpc Tokenize(TokenizeRequest) returns (TokenizeResponse);

  // 创建一次性支付 token（payment-core 在 Confirm 时调）
  rpc CreatePaymentToken(CreatePaymentTokenRequest) returns (CreatePaymentTokenResponse);

  // 用 payment token 换 PAN（**仅** card-payment 服务调）
  rpc Detokenize(DetokenizeRequest) returns (DetokenizeResponse);

  // 删卡（业务层 soft delete，物理失效靠 KMS rotate）
  rpc DeleteCard(DeleteCardRequest) returns (DeleteCardResponse);
}
```

### 2.4 审计

每次 Tokenize / Detokenize 必须 emit audit event：
```json
{
  "ts": "2024-05-05T...",
  "op": "detokenize",
  "kid": "v3",
  "pi_id": "pi_xxx",
  "user_id": "...",
  "caller_service": "card-payment",
  "caller_ip": "10.x.x.x",
  "trace_id": "..."
}
```
**不写 card-center 本地**（无 DB）→ 异步 push 到中央 audit Kafka topic。

## 3. card-payment：卡组织接口专用

### 3.1 为什么独立服务

PAN 只在最后一刻调 Visa/Mastercard 时需要。把这一刻包进一个专用服务：
- 网络层隔离：只有这个服务能出 DC 调卡组织
- 代码隔离：审计 / 加固 / pentest 范围最小
- 故障隔离：卡组织慢 / 错不影响主链路其它路径

### 3.2 数据持久化（极少）

card-payment 自有库只存：
```sql
CREATE TABLE card_transaction (
    id              BIGINT PRIMARY KEY,
    pi_id           VARCHAR(64),       -- 关联主 DC 的 PI
    network         VARCHAR(16),       -- visa / mastercard / jcb
    network_ref_no  VARCHAR(64),       -- 卡组织 transaction ID
    masked_pan      VARCHAR(20),       -- BIN(6) + last4
    amount          BIGINT,
    currency        VARCHAR(8),
    status          VARCHAR(16),       -- pending / approved / declined
    decline_code    VARCHAR(32),
    created_at      DATETIME(3),
    updated_at      DATETIME(3)
);
```

**严禁**写：full PAN, CVV, track data, 任何含 PAN 的 request/response body。

### 3.3 调用流程（典型 charge）

```
1. payment-core.Charge(pi_id) → 路由到 payment-channel.Charge
2. payment-channel.adapter[card] → card-payment.Authorize(pi_id, payment_token)
3. card-payment:
   ├─ pan := cardCenter.Detokenize(payment_token, pi_id)      // 仅这一刻有 PAN
   ├─ resp := visaAPI.Auth(pan, amount, ...)                  // 出 DC HTTPS
   ├─ pan = ""                                                // 立即清栈变量
   └─ persist(pi_id, network, masked_pan, network_ref, status, ...)
4. card-payment 返回 (network_ref, status) 给 payment-channel
5. payment-channel 返回 ChargeResponse 给 payment-core
```

PAN 生命周期：步骤 3 内 < 1ms，**永不离开 card-payment 进程内存**。

### 3.4 卡组织 adapter

每个 network 一个 adapter：
- visa（VisaNet API）
- mastercard（MIP / MIP-CE）
- jcb
- amex
- unionpay（CUP）

每个 adapter 实现：
```go
type Network interface {
    Authorize(ctx, AuthRequest) (AuthResponse, error)
    Capture(ctx, CaptureRequest) (CaptureResponse, error)
    Refund(ctx, RefundRequest) (RefundResponse, error)
    Void(ctx, VoidRequest) (VoidResponse, error)
    Query(ctx, QueryRequest) (QueryResponse, error)
}
```

参考 payment-channel 现有的 15 个 adapter 结构。

## 4. 网络隔离

### 4.1 监听端口

card-center / card-payment **只监听 HTTPS**：
- 入站：`:9443` (mTLS gRPC, TLS 1.2+, ECDHE-RSA-AES256-GCM-SHA384 优先)
- 入站：`:8443` (admin HTTP, mTLS)
- 不开 9090 / 9091 等明文端口

### 4.2 VPC / 防火墙

```
Card DC inbound rules:
  - allow: payment-channel (in main DC) → card-payment:9443
  - allow: card-payment → card-center:9443
  - deny: 其它一切

Card DC outbound rules:
  - allow: card-payment → visanet.com:443 / mastercard.com:443 / ...
  - allow: card-{center,payment} → kms-manage:9290 (mTLS)
  - allow: card-{center,payment} → kafka-audit:9093 (mTLS, audit log)
  - deny: 其它一切（含 DNS 走内部 resolver 白名单）
```

### 4.3 mTLS 证书签发

cert-manager + 自有 CA：
- card-center 持 server cert + client cert（调 KMS / Kafka 时用）
- card-payment 同上
- payment-channel（main DC）持 client cert，CN 白名单匹配
- 证书 90 天自动 rotate

## 5. 部署 layout

```
card-dc/
├── docker-compose.yml       # 单 DC 内的服务拓扑
├── kms-manage              # 卡 DC 内独立的 KMS 实例（不复用 main DC 的）
├── card-center             # 2-3 副本
└── card-payment            # 2-3 副本
```

注意：**KMS 也要在 card DC 内独立部署**。如果用 main DC 的 KMS，PAN 加解密
请求要跨 DC，违反隔离原则；且 main DC 的 KMS 在 SAQ-A-EP scope，把 SAQ-D scope
混进去。

## 6. 与现有服务的集成点

### 6.1 user-merchant-core

加一个 `user_card` 表（**不存 PAN**）：
```sql
CREATE TABLE user_card (
    id            BIGINT PRIMARY KEY,
    user_id       BIGINT,
    stored_token  VARCHAR(512),        -- card-center 返的存卡 token
    masked_pan    VARCHAR(20),         -- BIN+last4 给前端展示
    network       VARCHAR(16),
    exp_month     TINYINT,
    exp_year      SMALLINT,
    holder_name   VARCHAR(64),
    deleted_at    DATETIME(3) NULL,    -- soft delete
    created_at    DATETIME(3)
);
```

存卡流程：用户输卡（前端 → card-center 直传，绕过主 DC）→ card-center 返 token →
user-merchant-core 写 user_card 行。

### 6.2 order-core PI 创建

PI 创建带 `user_card_id` 字段。confirm PI 时：
1. order-core 取出 user_card 的 stored_token
2. order-core 调 card-center.CreatePaymentToken(stored_token, pi_id) 拿 30min 一次性 token
3. order-core 把 payment_token 传给 payment-core / payment-channel
4. payment-channel.adapter[card] 把 payment_token 传给 card-payment
5. card-payment 调 card-center.Detokenize → 拿 PAN → 调 Visa

### 6.3 payment-channel

加一个 `card` adapter（跟现有 15 个 adapter 同形），但 ChannelName="card"。
内部 dial card-payment:9443 over mTLS。

## 7. PCI Scope 映射

| 服务 | Scope |
|---|---|
| card-center | **SAQ-D** (CDE) |
| card-payment | **SAQ-D** (CDE) |
| 卡 DC 的 KMS | **SAQ-D** (CDE) |
| 卡 DC 的网络设备 | **SAQ-D** (CDE) |
| api-gateway | SAQ-A-EP（透传 token，不见 PAN） |
| payment-channel / order-core / 等 | SAQ-A-EP |
| user-merchant-core | SAQ-A-EP |

CDE = Cardholder Data Environment。CDE 范围越小，合规越省力。

## 8. 跟非卡渠道的关系

| Channel | 路由 |
|---|---|
| GCash / Maya / GrabPay / ShopeePay / Coins.ph | payment-channel.adapter[wallet] 直接走，不经 card-payment |
| BPI / BDO / Metrobank / Landbank | payment-channel.adapter[bank] 直接走 |
| Pesonet / Instapay | payment-channel.adapter[interbank] 直接走 |
| **Visa / Mastercard / JCB / AMEX** | payment-channel.adapter[card] → **card-payment** |

只有卡支付链路触发 PCI 合规要求。

## 9. 落地 Phase

```
Phase 1 (1 月)  ：card-center 服务 + KMS 集成 + Tokenize/Detokenize 测试
Phase 2 (1 月)  ：card-payment 服务 + 1 个 network adapter (Visa)
Phase 3 (2 周) ：mTLS / 防火墙 / 隔离 DC 部署
Phase 4 (2 周) ：order-core / user-merchant-core 接入 card-center API
Phase 5 (1 月)  ：剩余 4 个 network adapter (Mastercard / JCB / AMEX / UnionPay)
Phase 6 (持续) ：ASV 季度扫描 / 年度 pentest / SAQ-D 自评 / QSA 审计
```

总投入：2 后端 + 1 SRE + 1 安全工程师 ≈ 6 个月 + 持续运营。

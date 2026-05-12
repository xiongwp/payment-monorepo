# tokenization-vault

PCI 边界内的网络代币 vault. **双层代币 (Two-Layer Token)** 架构.

## 为什么需要这个

- **PCI 范围缩减**: 商户 DB 永远不存 PAN, 进 SAQ-D 从重资产降级
- **合规强制**: Visa Mandate / Mastercard Mandate / PCI-DSS 4.0 要求 recurring 卡用 network token
- **生命周期一致**: issuer 换卡 / 卡过期 → VTS/MDES 自动 push 新 PAN, vault 内 token 不变, 商户无感

## 双层代币

```
┌──────────────────────────────────────────────────────────┐
│                  商户 DB / 业务 SDK                      │
│  保存:  internal_token = "tk_live_abc123..."             │
└──────────────────────────────────────────────────────────┘
                            │ POST /v1/tokens/{tk}/charge
                            ▼
┌──────────────────────────────────────────────────────────┐
│  tokenization-vault (PCI scope, mTLS only)               │
│                                                          │
│  internal_token ─────────┐                               │
│                          │ lookup                        │
│   ┌──────────────────────▼────────────────────────────┐  │
│   │ InternalToken record:                             │  │
│   │   pan_hash (sha256)                               │  │
│   │   encrypted_pan (AES-256-GCM by DEK ⊃ KEK by KMS) │  │
│   │   NetworkRef { vts | mdes, network_token, exp }   │  │
│   └─────────────────┬──────────────────┬──────────────┘  │
│                     │                  │                 │
│   provision once    │                  │ each charge     │
│           ▼         │                  ▼                 │
│   ┌───────────────────┐    ┌────────────────────────┐    │
│   │ provider.Provision│    │ provider.Cryptogram    │    │
│   │   (VTS/MDES API)  │    │   amount, currency,    │    │
│   │   PAN → DPAN      │    │   recurring → TAVV     │    │
│   └───────────────────┘    │   eci, atc             │    │
│                            └────────────────────────┘    │
│                                       │                  │
└───────────────────────────────────────┼──────────────────┘
                                        ▼
                       ┌──────────────────────────────┐
                       │  PSP / acquirer              │
                       │  上送卡组 (Visa/MC):         │
                       │   - network_token (DPAN)     │
                       │   - cryptogram (TAVV/UCAF)   │
                       │   - ECI / ATC                │
                       └──────────────────────────────┘
```

## API

### 内部 (mTLS 内调用, PCI 边界)

| 路由 | 用途 |
|---|---|
| `POST /v1/tokens/exchange` | PAN → internal_token (PCI, 只在受控网络) |
| `POST /v1/tokens/{token}/charge` | 出 (network_token, cryptogram) 给 PSP |
| `POST /v1/tokens/{token}/provision` | 触发/重试 VTS/MDES provision |
| `GET /v1/tokens/{token}` | meta — last4 / brand / status (不返 PAN) |
| `POST /v1/tokens/{token}/suspend` | 冻结 |
| `DELETE /v1/tokens/{token}` | 软删 |

### Admin (X-Admin-Token)

| 路由 | 用途 |
|---|---|
| `GET /admin/health/providers` | provider 状态 |
| `GET /admin/metrics/merchants/{id}/count` | 商户当前 active token 数 |

## providers

| name | brand | 何时用 | impl |
|---|---|---|---|
| **vts** | Visa | brand=visa | stub (生产换 VTS HTTPS) |
| **mdes** | Mastercard | brand=mastercard | stub (生产换 MDES JWS) |
| **inhouse** | 其他 | brand=amex/jcb/discover/unknown/fallback | 自建 PCI vault, 推卡组用真 PAN |

## CIT vs MIT

- **CIT** (Customer-Initiated Transaction): `recurring=false`, 用户在场, ECI=05 (visa SCA 强通)
- **MIT** (Merchant-Initiated Transaction): `recurring=true`, 后台扣款, ECI=07 (no SCA, 走 stored credential)

## 部署

```bash
cd ../payment-admin-web
./deploy.sh up tokenization-vault
curl http://localhost:18089/healthz
bash ../tokenization-vault/test/smoke.sh http://localhost:18089
```

## 单测

```bash
make test
```

覆盖:
- exchange new token + Luhn 拒绝
- merchant-scoped dedup (同 PAN 不同商户得到不同 token)
- ChargeIntent 产生 cryptogram + ECI
- CIT/MIT ECI 不同
- suspended token 拒绝 charge

## 不在 scope (下一步)

- MySQL store (分片按 sha256(internal_token)[:N])
- KMS envelope DEK (现 dev 用 deterministic key)
- 真 VTS/MDES HTTPS transport
- VTS push notifications (卡过期 / suspended cards)
- card-payment 改造调 vault 而非自管 PAN
- subscription 续费用 MIT cryptogram

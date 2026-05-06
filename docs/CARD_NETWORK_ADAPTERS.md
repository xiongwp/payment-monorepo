# Card Network Adapters — 上线 runbook

5 个 network adapter（visa / mastercard / jcb / amex / unionpay）已写完，
**等卡组织合同到位后只需配置即可上线**。本文是从合同签好到生产灰度的清单。

## 路径概览

```
processor.Authorize
   ↓ req.Network 选 adapter
   ↓
adapter.Authorize  ──HTTPS+mTLS+签名──>  卡组织
   ↑                                        ↓
   └── 解码响应 → 风险字段映射 ←─────────────┘
```

各 adapter 共享 `internal/adapter/httpx/` 工具：
- `client.go` — mTLS HTTPS client + 重试 (5xx/408/429) + 连接池
- `signing.go` — RSA-SHA256 / HMAC-SHA256 / Visa canonical request 工具
- `risk.go` — AVS / CVV / 3DS / FraudScore / decline_code 归一

mock 模式（`endpoint=""` 或 `env != prod`）按 BIN 决定结果，5 家共 ~25 条规则。
完整 mock server 在 `cmd/mock-network/`，单进程模拟全部 5 家 REST API。

## 各家协议

| 网络 | 协议 | 鉴权 | endpoint 形态 |
| --- | --- | --- | --- |
| **Visa** | CyberSource REST v2 | mTLS + RSA HTTP Signature | `https://api.visa.com` (prod) / `apitest.cybersource.com` (sandbox) |
| **Mastercard** | MPGS REST v76 | mTLS + Basic Auth (`merchant.<m>:<password>`) | `https://<region>.gateway.mastercard.com` |
| **JCB** | J/Smart REST v1 | mTLS + HMAC-SHA256 | acquirer 提供 |
| **AmEx** | Direct API v2 | mTLS + HMAC-SHA256 | `https://api.americanexpress.com` (OptBlue 走 acquirer) |
| **UnionPay** | UPI v5.1 | optional mTLS + form RSA-SHA256 | `https://gateway.95516.com` |

## 上线流程（以 Visa 为例）

### 1. 拿到合同 / 凭证后

Visa 会下发：
- Merchant ID（CyberSource）
- API Key ID
- RSA 私钥（PKCS#8 PEM）
- mTLS client cert + key（Visa MAS）
- Visa CA root cert

### 2. 把凭证存到 KMS / Vault

不能进 env / config file 明文。建议：
- private_key、api_password、api_secret 走 KMS Decrypt 启动期取
- mTLS cert 路径可以是文件（cert-manager 投递）

### 3. 配置 card-payment

`config.yaml` 或 env：

```yaml
env: prod
network:
  visa:
    endpoint: "https://api.visa.com"
    merchant_id: "MID_VISA_PROD_xxx"
    api_key_id: "VKEY_xxx"
    private_key_path: "/etc/card-payment/secrets/visa/private.pem"
    client_cert: "/etc/card-payment/secrets/visa/client.crt"
    client_key:  "/etc/card-payment/secrets/visa/client.key"
    server_ca:   "/etc/card-payment/secrets/visa/visa-ca.crt"
    timeout: 30s
```

### 4. 启动期校验

`assertProdSafety` 在 prod 自动拒：
- endpoint 不 https
- endpoint 指 localhost / 127.x / .local / .internal（防 mock 误用）
- insecure_sandbox=true

并验证 mTLS / private_key 三件套能加载。任何一项缺失 → fx fail-fast 起不来。

### 5. 灰度 sandbox → prod

1. 先用 sandbox endpoint 跑全套交易测试（authorize / capture / refund / void / inquiry）
2. 用 mock-network 跑 e2e 失败场景（HARD/SOFT/pending）
3. prod 起来 0.1% 灰度（per-mch_id flag 在 risk-manage 控制）
4. 盯 metrics:
   - `paycard_authorize_total{network=visa,result=*}`
   - `paycard_authorize_duration_seconds{network=visa}`
   - `paycard_network_error_total{network=visa,kind=*}`
5. 24h 无异常后逐步放量

## 风险字段语义

每个 adapter 的 `Authorize` 都把响应中的风险字段映射到 `processor.NetworkAuthResponse`：

| 字段 | 取值 | 上层（risk-manage）用途 |
| --- | --- | --- |
| `AVSResult` | Y / A / N / U | 地址不匹配 → 升 review |
| `CVVResult` | M / N / P / U | CVV 不对 → 黑名单候选 |
| `ThreeDSStatus` | Y / A / N / U | 3DS 失败 → 拒（除非 merchant 接受 attempted） |
| `ThreeDSEci` | 02 / 05 / 01 / 06 / 07 | liability shift 判断 |
| `FraudScore` | 0-99 | ≥ 60 升 review；≥ 80 直接拒 |
| `DeclineCategory` | HARD / SOFT / "" | HARD = 永久拒 + 黑名单 48h；SOFT = 可重试 |

processor.Authorize 看到 `DeclineCategory == "HARD"` 自动 audit 警示日志。
risk-manage 据此把 user_id / card_token 加入黑名单候选。

## Mock server 用法

dev / e2e / 沙箱 staging 都可以用：

```bash
# 起 mock（自签 TLS，dev 用）
go run ./cmd/mock-network --addr :9555

# 或起 HTTPS（生成自签 cert）
openssl req -x509 -newkey rsa:2048 -keyout mock.key -out mock.crt -days 365 -nodes -subj "/CN=localhost"
go run ./cmd/mock-network --addr :9555 --cert mock.crt --key mock.key
```

card-payment 配置（dev only）：

```yaml
env: dev
network:
  visa:
    endpoint: "https://localhost:9555/visa"
    insecure_sandbox: true   # 跳过自签证书验证
```

mock BIN 表（5 家共用）：

```
4242 / 5454 / 5555 / 3528 / 3589 / 3782 / 3714 / 6225+ → approved (happy)
4000 / 5100 / 3500 / 3700 / 6225                       → declined SOFT
4001 / 5101 / 3501 / 3701 / 6226                       → declined HARD
4002                                                    → pending
```

## 已知留白（合同签后再补）

- Visa MLE (Message Level Encryption) — 部分高敏地区强制；目前未启用
- Mastercard MPGS PUT 严格化 — httpx 暂用 PostJSON（MPGS 实测兼容）
- 银联 PAN 字段加密（应用层 RSA） — 当前明文走 mTLS；接通时改加密
- AmEx OptBlue ARC 字段 — 由收单行配，当前留空
- 5 家 webhook 异步通知（先用同步 inquiry 兜底）

## 提交清单

```
adapter/httpx/client.go      共享 mTLS HTTP client
adapter/httpx/signing.go     RSA / HMAC / canonical 签名
adapter/httpx/risk.go        AVS/CVV/3DS/FraudScore 归一
adapter/visa/visa.go         CyberSource REST + RSA HTTP Signature
adapter/mastercard/...go     MPGS + Basic Auth
adapter/jcb/jcb.go           J/Smart + HMAC
adapter/amex/amex.go         Direct API + HMAC
adapter/unionpay/...go       UPI form + RSA
cmd/mock-network/main.go     5-in-1 mock server
docs/CARD_NETWORK_ADAPTERS.md  本文
```

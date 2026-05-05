# card-center

PCI-DSS SAQ-D scope 的 token vault 服务。**无数据库**：token 本身就是 PAN 的
KMS-encrypted 密文。

## 关键约束

- 部署在隔离 datacenter（跟 card-payment 一起）
- 仅监听 mTLS gRPC `:9443`，明文端口禁开
- 出站只到 KMS（独立 DC 内部）+ Kafka audit topic（独立 DC 内部）
- 客户端证书 CN 白名单：
  - `Tokenize` / `CreatePaymentToken`：order-core / user-merchant-core / 前端 SDK
  - `Detokenize`：仅 card-payment（其它一律拒）
- 每次 op emit audit event 到 Kafka，不写 card-center 本地

## Token 类型

```
存卡 token (long-lived)：
   tok_card_<base64url(KMS.Encrypt({pan,exp,holder,nonce,iat}, AAD="card:user_id:<uid>"))>

支付 token (一次性, TTL 30min)：
   tok_pay_<base64url(KMS.Encrypt({pan,pi_id,exp_ts,nonce,iat,...}, AAD="pay:pi:<pi_id>"))>
```

AAD 让 token 跟 user_id / pi_id 强绑定，被偷接到别人的 user / PI 直接 KMS Decrypt 失败。

## 调用流程

```
[user 存卡]
  前端 SDK ──HTTPS──→ card-center.Tokenize(pan, user_id) → stored_token
  user-merchant-core 存 user_card 表（不见 PAN，只见 token）

[user 付款]
  order-core ──mTLS──→ card-center.CreatePaymentToken(stored_token, pi_id)
                                 ↓
                       payment_token (TTL 30min)
                                 ↓
  payment-channel.adapter[card] ──mTLS──→ card-payment.Authorize(payment_token, pi_id)
                                                   ↓
                                  card-payment ──mTLS──→ card-center.Detokenize(payment_token, pi_id)
                                                                    ↓
                                                            PAN (仅 RPC 栈内存)
                                                                    ↓
                                                  card-payment ──HTTPS+VPN──→ Visa Net
                                                                    ↓
                                                            (PAN 立即清栈)
```

## 部署

```yaml
# 独立 DC，独立 VPC，防火墙白名单
services:
  card-center:
    image: card-center:latest
    ports: ["9443:9443"]
    volumes:
      - /etc/card-center/tls:/etc/tls:ro     # cert-manager 注入
    environment:
      CARDCENTER_ENV: "prod"
      CARDCENTER_TLS_CERT: "/etc/tls/server.crt"
      CARDCENTER_TLS_KEY: "/etc/tls/server.key"
      CARDCENTER_TLS_CLIENT_CA: "/etc/tls/client-ca.crt"
      CARDCENTER_KMS_ENDPOINT: "kms-card:9290"     # 独立 DC 内的 KMS
      CARDCENTER_AUDIT_KAFKA_BROKERS: "audit-kafka:9093"
    networks: [card-internal]

networks:
  card-internal:
    driver: bridge
    internal: true   # 不接外网，走专门的 ingress / VPN 出 DC
```

## 文件 layout

```
packages/card-center/
├── api/proto/cardcenter/v1/cardcenter.proto    # gRPC API 定义
├── cmd/server/main.go                          # 启动入口（mTLS-only）
├── internal/
│   ├── vault/vault.go                          # 纯密码学 token 实现（无 DB）
│   ├── vault/vault_test.go                     # 单测：Tokenize/Detokenize/AAD校验/TTL
│   ├── kmsclient/client.go                     # mTLS 调 kms-manage（待加）
│   ├── audit/kafka_emitter.go                  # 异步 emit audit event（待加）
│   └── server/grpc.go                          # gRPC handler + clientCN 白名单 interceptor（待加）
└── README.md
```

## 待补（最小完整版上线前必做）

- [ ] `internal/kmsclient` mTLS gRPC 客户端调 kms-manage
- [ ] `internal/audit` Kafka producer + 失败重试 + dead-letter
- [ ] `internal/server` gRPC handler + clientCN 白名单 interceptor
- [ ] 启动期 TLS cert 刚好的过期检查（< 30 天告警）
- [ ] `/healthz` `/readyz` （mTLS）
- [ ] Prometheus 指标：tokenize_total / detokenize_total / kms_latency / audit_lag
- [ ] Dockerfile + e2e 测试

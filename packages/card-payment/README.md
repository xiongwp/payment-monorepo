# card-payment

跟卡组织（Visa / Mastercard / JCB / AMEX / UnionPay）通信的专用服务。
**唯一**会拿到 PAN 的服务。

## 关键约束

- 部署在隔离 datacenter（跟 card-center 一起，SAQ-D scope）
- 仅监听 mTLS gRPC `:9443`
- 出站只允许：HTTPS 到卡组织 endpoint + mTLS 到 card-center + mTLS 到 KMS + mTLS 到 audit Kafka
- 调用方 CN 白名单：仅 payment-channel
- DB 仅存 `masked_pan`（BIN+last4）+ network_ref_no + amount + status；**严禁**存 PAN / CVV / track data

## PAN 生命周期纪律（< 1ms）

```
[Authorize handler 内]
  detok = cardCenter.Detokenize(payment_token, pi_id)   ← PAN 进栈
  authReq.PAN = detok.PAN                                ← 仅传 network adapter
  resp = network.Authorize(authReq)                      ← adapter 发 HTTPS
  defer { detok.PAN = ""; authReq.PAN = "" }             ← 退栈前清掉
  repo.Insert(masked_pan, network_ref_no, ...)           ← 持久化不带 PAN
  return (network_ref_no, status)                        ← 上抛只有 token-level 信息
```

代码层守则：
- 任何 `logger` 调用列字段名（**禁** `zap.Any(req)`，会带出 PAN）
- `defer { variable = "" }` 主动清栈（Go 没强 zero memory，但保证不再被引用）
- `internal/processor/` 是**唯一**接触 `Detokenized.PAN` 的代码路径
- pre-commit lint：`grep -rn "PAN\|pan" internal/` 看到非预期出现位置报警

## 文件 layout

```
packages/card-payment/
├── api/proto/cardpayment/v1/cardpayment.proto       # gRPC API
├── cmd/server/main.go                               # mTLS-only 启动 + prod safety
├── internal/
│   ├── processor/processor.go                        # 核心：detok → network → mask
│   ├── adapter/
│   │   ├── visa/visa.go                              # Visa Net adapter（待加）
│   │   ├── mastercard/mastercard.go                  # MIP / MIP-CE adapter（待加）
│   │   ├── jcb/jcb.go                                # 待加
│   │   ├── amex/amex.go                              # 待加
│   │   └── unionpay/unionpay.go                      # 待加
│   ├── cardcenterclient/client.go                   # mTLS gRPC 调 card-center（待加）
│   ├── repo/card_transaction.go                     # GORM repo，只存 masked（待加）
│   └── server/grpc.go                               # gRPC handler + clientCN 白名单 interceptor（待加）
├── go.mod
└── README.md
```

## DB schema（独立 DC 内的 MySQL）

```sql
CREATE TABLE card_transaction (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    pi_id           VARCHAR(64)  NOT NULL,
    network         VARCHAR(16)  NOT NULL,
    network_ref_no  VARCHAR(64),
    masked_pan      VARCHAR(20)  NOT NULL,        -- BIN+last4
    amount          BIGINT       NOT NULL,
    currency        VARCHAR(8)   NOT NULL,
    status          VARCHAR(16)  NOT NULL,        -- approved / declined / pending / error
    decline_code    VARCHAR(32),
    idempotency_key VARCHAR(128),
    created_at      DATETIME(3)  DEFAULT CURRENT_TIMESTAMP(3),
    updated_at      DATETIME(3)  DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
    UNIQUE KEY uk_pi_idem (pi_id, idempotency_key),
    KEY idx_network_ref (network_ref_no),
    KEY idx_pi (pi_id)
) ENGINE=InnoDB CHARSET=utf8mb4;
```

注意字段名只许 `masked_pan`，不许有 `pan` / `card_number` / `pan_full` 字段。
schema migration lint 拒绝任何含 `pan` 但不是 `masked_pan` 的字段。

## 部署

```yaml
# 独立 DC 的 docker-compose.yml
services:
  card-payment:
    image: card-payment:latest
    ports: ["9443:9443"]
    volumes:
      - /etc/card-payment/tls:/etc/tls:ro
    environment:
      CARDPAYMENT_ENV: "prod"
      CARDPAYMENT_TLS_CERT: "/etc/tls/server.crt"
      CARDPAYMENT_TLS_KEY: "/etc/tls/server.key"
      CARDPAYMENT_TLS_CLIENT_CA: "/etc/tls/client-ca.crt"
      CARDPAYMENT_CARD_CENTER_ENDPOINT: "card-center:9443"
      CARDPAYMENT_NETWORK_VISA_ENDPOINT: "https://api.visa.com/..."
      CARDPAYMENT_DATABASE_DSN: "..."
    networks: [card-internal]
    restart: unless-stopped

networks:
  card-internal:
    internal: true   # 不接外网；走专门的 ingress 出 DC
```

## 待补（最小完整版上线前必做）

- [ ] `internal/cardcenterclient` mTLS gRPC 调 card-center
- [ ] `internal/adapter/visa` 一个完整 Visa Net adapter（其它 4 个 network 同模板复刻）
- [ ] `internal/repo/card_transaction` GORM 实现 + 单测
- [ ] `internal/server` gRPC handler + clientCN 白名单 interceptor
- [ ] Capture / Refund / Void / Query 的 Processor 方法（PAN-free，基于 network_ref_no）
- [ ] 3DS 集成（外部 3DS server / 自建 3DS Requestor）
- [ ] Dockerfile + e2e 测试

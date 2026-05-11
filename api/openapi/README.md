# Payment Platform — OpenAPI 3.0 Specs

10 份 spec 描述全部对外 / 内部 HTTP 接口。

## Specs

| Service | Spec | Port |
|---|---|---|
| Payment Gateway | [payment-gateway.yaml](./payment-gateway.yaml) | 18091 |
| Refund Engine | [refund-engine.yaml](./refund-engine.yaml) | 18094 |
| Dispute Service | [dispute-service.yaml](./dispute-service.yaml) | 18092 |
| Billing System | [billing-system.yaml](./billing-system.yaml) | 18090 |
| Merchant Webhook | [merchant-webhook.yaml](./merchant-webhook.yaml) | 18093 |
| KYC / KYB | [kyc-service.yaml](./kyc-service.yaml) | 18095 |
| Audit Log | [audit-log.yaml](./audit-log.yaml) | 18096 |
| OAuth2 Server | [oauth2-server.yaml](./oauth2-server.yaml) | 18087 |
| Biz Admin Web | [biz-admin-web.yaml](./biz-admin-web.yaml) | 18099 |
| Payment MW (lib) | [payment-mw.yaml](./payment-mw.yaml) | — |

共享组件: [`_common.yaml`](./_common.yaml) — securitySchemes / Error / Money / Pagination。

## 本地浏览

```bash
# 任何静态 HTTP server 都行
cd api/openapi
python3 -m http.server 8000
open http://localhost:8000/index.html
```

Swagger UI 自动加载 + 顶部导航在 10 个 spec 间切换。

## 校验 (CI)

```bash
# 安装
npm i -g @redocly/cli @stoplight/spectral-cli

# lint
spectral lint api/openapi/*.yaml

# 校验 $ref 引用
redocly lint api/openapi/oauth2-server.yaml
```

## 用 / 不用 OAuth2

OAuth 2.0 client_credentials 是首选方式。每个 spec 都声明:
```yaml
security:
  - oauth2BearerJWT: [scope1, scope2]
```

遗留 X-API-Key / X-Internal-Token 仍可用 (兼容期)，配 `apiKey` / `internalToken`
schemes。生产推荐 **所有商户 / 服务间调用走 OAuth2**。

## scope 矩阵

| Scope | 含义 | 用在 |
|---|---|---|
| `charge:write` / `charge:read` | 交易写/读 | gateway |
| `refund:write` / `refund:read` | 退款写/读 | refund-engine |
| `dispute:write` / `dispute:read` | 争议写/读 | dispute-service |
| `kyc:write` / `kyc:read` | KYC 写/读 | kyc-service |
| `merchant:write` / `merchant:read` | 商户配置 | webhook / merchant-core |
| `ops:write` / `ops:read` | ops 后台 | biz-admin-web / audit |

## 集成 OAuth2 到服务 (Go)

```go
package main

import (
    "os"
    mw "reconcile-system/packages/payment-mw"
)

func main() {
    mw.Bootstrap(mw.BootstrapConfig{
        ServiceName: "refund-engine",
        DefaultPort: "8080",
        SetupRoutes: setupRefundRoutes,
        // BearerJWT 自动从 OAUTH2_JWKS_URL env 装载
    })
}
```

env:
```bash
OAUTH2_JWKS_URL=http://oauth2-server:8087/.well-known/jwks.json
OAUTH2_ISSUER=http://oauth2-server:8087
OAUTH2_AUDIENCE=payment-api
```

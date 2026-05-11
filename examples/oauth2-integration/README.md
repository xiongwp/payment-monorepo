# OAuth2 Integration Demo

一个可运行的端到端 demo: 商户 → OAuth → API → 服务间调用全打通。

## 涉及组件

```
┌─────────────┐         POST /oauth2/token         ┌─────────────┐
│  Merchant   │ ─────── client_credentials ──────► │  oauth2-    │
│   client    │                                    │  server     │
│   (SDK)     │ ◄────── access_token (JWT) ─────── │  :8087      │
└──────┬──────┘                                    └─────────────┘
       │                                                  ▲
       │ POST /api/v1/refunds                             │ GET /.well-known/jwks.json
       │ Authorization: Bearer <jwt>                      │ (resource servers fetch)
       ▼                                                  │
┌─────────────┐                                    ┌──────┴──────┐
│  refund-    │ ─── verify with JWKS ────────────► │  payment-mw │
│  engine     │                                    │  JWTVerifier│
│  :8094      │                                    └─────────────┘
│  (resource  │
│   server)   │ ─── POST /api/webhooks (Bearer) ─► merchant-webhook
└─────────────┘     (service-to-service)            (also resource server)
```

## 文件

```
examples/oauth2-integration/
├── README.md
├── 00_setup.sh            # 起 oauth2-server + 创建 demo client
├── 01_merchant_demo.go    # 商户 SDK 调用 demo (Go)
├── 02_service_demo.go     # 服务间调用 demo (Go)
├── 03_curl_flow.sh        # 纯 curl 复现完整流程
├── 04_resource_server.go  # 接入 OAuth2 的 demo resource server (最小)
└── 05_e2e_test.sh         # 端到端集成测试
```

## 运行 (3 分钟跑完)

```bash
# 0. 起 oauth2-server (本地)
docker run -d --name oauth2-demo \
  -p 8087:8087 \
  -e OAUTH2_DEV_SEED=1 \
  -e OAUTH2_ADMIN_TOKEN=admintok \
  -e OAUTH2_ISSUER=http://localhost:8087 \
  oauth2-server:local

# 1. 起 demo resource server (展示 mw.Auth 集成)
cd examples/oauth2-integration
OAUTH2_JWKS_URL=http://localhost:8087/.well-known/jwks.json \
OAUTH2_ISSUER=http://localhost:8087 \
OAUTH2_AUDIENCE=payment-api \
  go run ./04_resource_server.go &

# 2. 跑商户 demo (拿 token + 调 API)
go run ./01_merchant_demo.go

# 3. 跑服务间 demo
go run ./02_service_demo.go

# 4. e2e 测试
./05_e2e_test.sh
```

## 三种集成方式

### A. Go 业务服务接入（推荐 — 用 payment-mw）

只需一行 env，`mw.Bootstrap()` 自动接 OAuth2:
```bash
OAUTH2_JWKS_URL=http://oauth2-server:8087/.well-known/jwks.json
OAUTH2_ISSUER=http://oauth2-server:8087
OAUTH2_AUDIENCE=payment-api
```
业务代码加 scope check:
```go
mux.Handle("/api/v1/refunds",
    mw.RequireScope("refund:write")(
        http.HandlerFunc(createRefund),
    ))
```

### B. 非 Go 服务 / 老服务接入

调 `POST /oauth2/introspect` 委托验签（适合 PHP/Python/Java 简单接入）:
```bash
curl -X POST http://oauth2-server:8087/oauth2/introspect \
     -d "token=$TOKEN" \
     -d "client_id=svc_xxx" -d "client_secret=xxx"
# → {"active":true, "scope":"refund:write", ...}
```
缺点: 每个请求多一跳；优化: 本地缓存 1min。

### C. 客户端拿 token

```go
tc := client.New(client.Config{
    TokenURL:     "http://oauth2-server:8087/oauth2/token",
    ClientID:     os.Getenv("PAYMENT_CLIENT_ID"),
    ClientSecret: os.Getenv("PAYMENT_CLIENT_SECRET"),
    Scope:        "refund:write",
})
req, _ := http.NewRequest("POST", "http://refund-engine:8080/api/v1/refunds", body)
resp, err := tc.Do(req)  // 自动加 Bearer header + 自动 refresh
```

## scope 推荐配置

| 服务 | client_id 前缀 | 推荐 scope |
|---|---|---|
| 商户 prod | `mer_<merchant_id>_prod` | `charge:write refund:read` |
| 商户 sandbox | `mer_<merchant_id>_test` | `charge:write refund:write dispute:read` |
| 内部服务 payment-gateway | `svc_payment_gateway` | `charge:write refund:read dispute:read` |
| 内部服务 billing-system | `svc_billing_system` | `charge:read refund:read` |
| ops 后台 | `ops_admin_<email>` | `ops:read ops:write merchant:read` |

## 常见集成陷阱

1. **kid 不匹配** → JWKS 拉取失败；查 jwks.json 输出，确认 oauth2-server 起来了
2. **iss/aud mismatch** → resource server env 跟 oauth2-server 配置不一致
3. **token 过期** → SDK 没自动 refresh，检查 `RefreshSkew` 是否合理（默认 5min）
4. **scope 不足返 403** → admin 创建 client 时 `allowed_scopes` 没含；rotate-secret 不会改 scope
5. **JWKS 缓存陈旧** → key rotation 后 JWTVerifier 每 10min 自动 refresh，急用调 `verifier.Stop(); NewJWTVerifier(...)` 或重启

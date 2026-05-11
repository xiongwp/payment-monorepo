# OAuth 2.0 + OpenAPI 3.0 Rollout

本会话完成: 一份完整的 **服务间 / 商户授权体系** + **API 文档体系**。

## 1. 新增/改动一览

### oauth2-server (新包，10 文件)

```
packages/oauth2-server/
├── cmd/server/main.go                     # 入口 (mysql/memory 双模式)
├── internal/domain/client.go              # Client / TokenRequest / Claims
├── internal/jwks/keystore.go              # RSA key 管理 + Sign / Verify / JWKS
├── internal/jwks/rs256.go                 # SignRS256 / VerifyRS256
├── internal/store/memory.go               # 内存 client store + bcrypt + scope intersect
├── internal/store/mysql.go                # MySQL 持久化
├── internal/adminhttp/server.go           # 7 endpoint (token/introspect/revoke/jwks/discovery/admin)
├── client/client.go                       # Go SDK (auto-refresh + retry)
├── api/openapi.yaml                       # OpenAPI 3.0 spec
├── database/init/01_schema.sql            # clients + revoked_tokens + rsa_keys + admin_audit
├── deploy/k8s/oauth2-server.yaml          # Deployment/Service/Ingress/PDB/NetworkPolicy
├── Dockerfile                             # multi-stage build
├── test/smoke.sh                          # e2e 烟雾测试 13 步
└── go.mod
```

**端点 (RFC 6749 / 7517 / 7662 / 7009 标准):**
- `POST /oauth2/token` — client_credentials → RS256 JWT
- `POST /oauth2/introspect` — 验签 + revocation 检查
- `POST /oauth2/revoke` — jti 黑名单
- `GET  /.well-known/jwks.json` — 公钥分发
- `GET  /.well-known/openid-configuration` — Discovery
- `POST /admin/clients` — 创建客户端（仅展示 secret 1 次）
- `POST /admin/clients/{id}/rotate-secret` — 滚 secret
- `POST /admin/clients/{id}/suspend` / `activate` / `revoke`
- `POST /admin/keys/rotate` — RSA key rotation（老 key 保留 30d 验签）

**关键设计:**
- RSA 2048 + RS256（业界标准）
- kid = SHA256(pubkey)[:8] base64url
- Active + retired keys（rotation 期老 token 仍可验）
- bcrypt(secret)，原始 secret 不落盘
- jti revocation（黑名单 TTL = token exp）
- IP 白名单 + scope 交集 + status check
- 后台 GC 每 15min 清 retired key / revocation

### payment-mw 升级 (Bearer JWT 验签)

```
packages/payment-mw/
├── oauth_bearer.go   # NEW: JWTVerifier (JWKS 拉取/缓存/refresh) + HasScope/RequireScope
├── mw.go             # MOD: AuthConfig.BearerJWT, Actor.{ClientID,ServiceID,Scopes}
└── bootstrap.go      # MOD: env OAUTH2_JWKS_URL 自动装 BearerJWT
```

**Auth 优先级 (按顺序):**
1. PublicPath → anonymous
2. `Authorization: Bearer <JWT>` + BearerJWT → OAuth2 (claims → Actor)
3. `X-API-Key` → 商户/ops (遗留)
4. `X-Internal-Token` → 内网服务 (遗留)
5. 都不匹配 → 401

**集成 (一行 env):**
```bash
OAUTH2_JWKS_URL=http://oauth2-server:8087/.well-known/jwks.json
OAUTH2_ISSUER=http://oauth2-server:8087
OAUTH2_AUDIENCE=payment-api
```
`mw.Bootstrap()` 自动检测这 3 个 env，启 JWTVerifier。

**业务代码 RBAC:**
```go
mux.Handle("/api/v1/refunds", mw.RequireScope("refund:write")(handler))
// 或
if !mw.HasScope(r.Context(), "refund:write") { http.Error(w, "forbidden", 403); return }
```

### OpenAPI 3.0 specs (10 份)

```
api/openapi/
├── _common.yaml             # 共享 securitySchemes + Error + Money + Pagination
├── payment-gateway.yaml
├── refund-engine.yaml
├── dispute-service.yaml
├── billing-system.yaml
├── merchant-webhook.yaml
├── kyc-service.yaml
├── audit-log.yaml
├── oauth2-server.yaml
├── biz-admin-web.yaml
├── payment-mw.yaml          # /healthz + /metrics + 横切 header
├── index.html               # Swagger UI portal (10 spec 切换)
└── README.md
```

每个 spec 都声明:
```yaml
security:
  - oauth2BearerJWT: [<scope>]
```

`oauth2BearerJWT` securityScheme 在 `_common.yaml` 集中定义（含全部 12 scope）。

### deploy 集成

```
packages/payment-admin-web/deploy/overrides/biz-stack.yml
  + oauth2-server service (port 18087, dev-seed 3 clients)
  + biz-admin-web 加 OAUTH2_BASE_URL env
  + volumes.oauth2-keys (RSA private key 持久化)
```

## 2. 端到端流程

### A. 商户接入

```
1. ops 在 biz-admin-web 创建 OAuth client:
   POST /admin/clients
   {
     "name": "Acme Corp",
     "owner_type": "merchant",
     "owner_id": "merchant_001",
     "allowed_scopes": "charge:write refund:write",
     "allowed_ips": "1.2.3.4",
     "expires_in_days": 365
   }
   → 返 { client_id, client_secret }  (仅展示 1 次!)

2. 商户后端配置:
   PAYMENT_CLIENT_ID = mer_merchant_001_xxx
   PAYMENT_CLIENT_SECRET = <secret>

3. 商户调 API:
   tc := oauth2client.New(...)
   req, _ := http.NewRequest("POST", "https://api.payment.example.com/gw/v1/payment-intents", body)
   tc.Do(req)  // 自动加 Authorization: Bearer <jwt>

4. payment-gateway 验签:
   - payment-mw.Auth 看 Authorization header
   - JWTVerifier.Verify(token) — 本地 RS256 验签（用 JWKS 缓存）
   - claims.owner_type=merchant → Actor.MerchantID = "merchant_001"
   - claims.scope 含 "charge:write" → RequireScope 通过
   - 业务逻辑跑

5. token 即将过期:
   - oauth2client SDK 提前 5min 自动 refresh
   - 重复 step 3
```

### B. 服务间调用

```
1. ops 创建 service client:
   POST /admin/clients
   { "owner_type": "service", "owner_id": "payment-gateway",
     "allowed_scopes": "charge:write refund:write dispute:read" }

2. payment-gateway pod 启动:
   env: PAYMENT_CLIENT_ID=svc_payment_gateway, PAYMENT_CLIENT_SECRET=<sealed-secret>

3. payment-gateway 调 refund-engine:
   refundClient.Do(req)  // 自动 OAuth2

4. refund-engine 验签 + scope check
   - claims.owner_type=service → Actor.ServiceID="payment-gateway"
   - mw.RequireScope("refund:write") 通过
```

### C. Key rotation (RSA)

```
T0:    /admin/keys/rotate
       → 新 kid=B; 老 kid=A 移入 retired
T0:    新 token 签发用 kid=B
T0~30d:旧 token (kid=A) 仍可验 (retired 池)
T+30d: PurgeRetired() 清掉 kid=A
       → 老 token 全部失效

商户/服务 SDK 自动从 JWKS endpoint 拉新公钥（10min refresh），透明无感。
```

## 3. 生产部署 checklist

- [ ] 生成强 RSA private key (生产用 cert-manager + KMS envelope encryption)
- [ ] `OAUTH2_ADMIN_TOKEN` 用 `head -c 32 /dev/urandom | base64` 生成，sealed-secrets 注入
- [ ] `OAUTH2_MYSQL_DSN` 配生产 MySQL（不用 in-memory）
- [ ] 删 dev seed (`OAUTH2_DEV_SEED` 不设)
- [ ] 关 admin endpoints 的公网入口（NetworkPolicy + ingress 限内网）
- [ ] 加 SLO 告警: token endpoint p99 > 200ms, 5xx rate > 0.1%
- [ ] cron 每 90d 调 `/admin/keys/rotate`
- [ ] cron 每天 GC `revoked_tokens` (where expires_at < NOW())
- [ ] 备份: RSA key + clients 表
- [ ] 监控: token 颁发量, introspect 量, JWKS 拉取量, 失败率

## 4. 验证

### 本地 e2e

```bash
# 1. 构建 + 起栈
cd packages/oauth2-server && docker build -t oauth2-server:local .
cd ../payment-admin-web/deploy && ./biz-build-and-up.sh up

# 2. 跑烟雾测试
cd packages/oauth2-server/test
OAUTH2_ADMIN_TOKEN=admintok-dev-CHANGE-IN-PROD \
  ./smoke.sh http://localhost:18087
```

### Swagger UI

```bash
cd api/openapi && python3 -m http.server 8000
open http://localhost:8000/index.html
```

## 5. 后续优化 (P2)

- OAuth client SDK 出 Python / Node / Java 版（除 Go 外）
- 加 mTLS + OAuth2 双重（已支持，文档化）
- 加 PKCE 流（如果将来支持用户授权）
- 加 refresh_token（如果上 user-facing flow）
- ABAC 替代 scope（更细颗粒度）
- 接 SIEM（admin_audit 表流到 SIEM）
- 限流: token endpoint per-client 100 rps
- token 缓存：resource server 本地 1min LRU 减 introspect 压力

---

参考标准:
- RFC 6749 §4.4 — client_credentials grant
- RFC 7515 — JWS
- RFC 7517 — JWK / JWKS
- RFC 7518 — JWA (RS256)
- RFC 7519 — JWT
- RFC 7591 — Dynamic Client Registration（未实现，admin API 替代）
- RFC 7662 — Token Introspection
- RFC 7009 — Token Revocation
- RFC 8725 — JWT BCP（alg whitelist / kid required）
- OpenAPI 3.0.3 — API specification

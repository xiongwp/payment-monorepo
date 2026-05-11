# CLAUDE.md — oauth2-server

简要给 AI agent 看的项目说明。

## 角色

OAuth 2.0 client_credentials 授权服务器 — 商户 / 内部服务调 payment 平台 API 时拿 Bearer token。

## 关键决策 (改前必读)

- **HTTP-only, 不暴露 gRPC** — 跟外部商户 / 服务通信走 HTTPS, 简化集成成本
- **RS256 + JWKS 分发** — 资源服务本地验签 (99% 路径 0 RTT), 仅 revocation 检查需查 introspect
- **client_credentials 唯一 grant** — 支付平台无用户授权场景, 拒绝 authorization_code / refresh_token 等
- **bcrypt(secret)** — 原始 secret 仅创建/rotate 时一次性返回, 不落盘明文
- **jti 黑名单 revocation** — 不依赖资源服务侧检查, 业务 token 想撤就撤
- **RSA key 90d rotation + 30d retention** — 老 token 仍可验, JWKS endpoint 列 active+retired 全部公钥
- **分库分表 100 shard** — clients / revoked_tokens / admin_audit 按 FNV-1a hash, 跟 u-m-c 同 layout

## 不做的事

- ❌ 用户授权流 (authorization_code grant)
- ❌ Refresh token (client_credentials 直接重发就行)
- ❌ Dynamic Client Registration (RFC 7591) — admin API 替代
- ❌ Token Exchange (RFC 8693)
- ❌ PKCE — 没 user-facing flow 用不上

## 跟谁联动

- **payment-mw**: BearerJWT 验签 + scope RBAC, 业务服务 1 行 env 接入
- **oauth2-server.client/**: Go SDK, 商户/内部服务复用 (auto-refresh + retry)
- **MySQL**: 客户端 / revocation / audit; 100 shard (跟 u-m-c 同 layout)
- **Prometheus**: 11 个 metric + 9 条告警

## 改这个服务时

1. 改 proto / API 改后端时 → 更新 `api/openapi.yaml`
2. 加新 scope → `internal/domain/client.go` + payment-mw scope.go
3. RSA / token 安全相关 → 必走 review
4. schema 变更 → 同步改 `database/init/01_schema_sharded.sql` + `generate.sh`
5. metrics → 同步 `deploy/prometheus/alerts.yaml`

## 测试

- 单元: `go test ./...` (覆盖 jwks / ratelimit / sharding)
- e2e: `bash test/smoke.sh` (13 步)
- 压测: `bash test/loadtest.sh` (3 场景)
- 集成: `examples/oauth2-integration/05_e2e_test.sh`

## 重大注意

- 改 RSA key rotation 周期 必须 ≤ retired_period, 否则历史 token 提早失效
- 改 scope 命名约定 (现 `resource:action`) 必须同步改 payment-mw + 全部 graph
- 改分片策略 (router) 必须 reshard, 见 user-merchant-core/SHARDING_MIGRATION.md
